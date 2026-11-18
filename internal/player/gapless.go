package player

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/render"
	"mygo-music/internal/music"
)

type nativeContinuation struct {
	slot   render.NextSlot
	token  uint64
	index  int // queue identity before traversal order is applied
	stream *atomic.Pointer[music.Stream]
}

func (p *playbackState) syncNativeBoundary() {
	if p.native != nil {
		p.adoptNativeBoundary(p.native.Snapshot(), true)
	}
}

func nativeGains(src music.Source, id string, info *music.Stream) dsp.Gains {
	if local, ok := src.(interface{ NativeReplayGain(string) dsp.Gains }); ok {
		return local.NativeReplayGain(id)
	}
	return dsp.Gains{Track: dsp.Gain{DB: info.Gain, Peak: info.Peak, Present: info.Gain != 0}}
}

func (p *playbackState) prepareNativeNext() {
	c := p.nativeNext
	if p.output.Mode == OutputDoP || p.output.Mode == OutputASIO {
		return
	}
	if c == nil || c.token != 0 || p.at < 0 || p.at >= len(p.order) || p.seeking || p.track == nil || p.track.Source != "local" {
		return
	}
	at := p.at + 1
	if p.repeat == RepeatOne {
		at = p.at
	} else if at == len(p.order) {
		if p.repeat != RepeatAll || p.feed != nil {
			return
		}
		at = 0
	}
	index := p.order[at]
	t := p.queue[index]
	if t.Source != "local" {
		return
	}
	src := p.sourceOf(&t)
	if src == nil || src.Info().Playback == music.PlaybackRemote {
		return
	}
	info := new(atomic.Pointer[music.Stream])
	p.nativeToken++
	if c.slot.Offer(render.NextMedia{Token: p.nativeToken, Source: t.Source, ID: t.ID, Title: t.Name,
		Resolve: func(ctx context.Context) (string, dsp.Gains, error) {
			if err := ctx.Err(); err != nil {
				return "", dsp.Gains{}, err
			}
			stream, err := src.Stream(t.ID, "lossless")
			if err != nil {
				return "", dsp.Gains{}, err
			}
			if stream == nil {
				return "", dsp.Gains{}, music.ErrUnavailable
			}
			path, ok := music.PathOf(stream.URL)
			if !ok {
				return "", dsp.Gains{}, errors.New("audio: continuation is not a local file")
			}
			info.Store(cloneStream(stream))
			return path, nativeGains(src, t.ID, stream), nil
		},
	}) {
		c.token, c.index, c.stream = p.nativeToken, index, info
	}
}

func (p *playbackState) adoptNativeBoundary(s engine.Snapshot, prepare bool) bool {
	c := p.nativeNext
	if c == nil || c.token == 0 || s.ContinuationToken != c.token || s.State == engine.Failed {
		return false
	}
	at := slices.Index(p.order, c.index)
	if at < 0 {
		return false
	}
	t := p.queue[c.index]
	if t.ID != s.Current.ID || t.Source != s.Current.Source {
		return false
	}
	p.recordCompletionAt(p.Duration())
	c.slot.Acknowledge(c.token)
	p.nativeState = s
	p.at, p.track, p.info = at, &t, c.stream.Load()
	c.token, c.stream = 0, nil
	p.lyrics, p.lyricsLoaded = nil, false
	p.pending, p.savedPosition = 0, 0
	p.recorded, p.nativeEnded = "", false
	p.gen++
	p.announce(&t, nil)
	p.loadCover(p.gen, &t)
	p.loadLyrics(p.gen, p.sourceOf(&t), t)
	if prepare {
		p.prepareNativeNext()
	}
	p.refill()
	p.sync()
	return true
}

func (p *playbackState) updateNativePlan() {
	c := p.nativeNext
	if c == nil {
		return
	}
	if !c.slot.Clear() && p.native != nil {
		// Once Render committed the next PCM, revocation requires purging
		// the output epoch. Adopt any already confirmed presentation first.
		if !p.adoptNativeBoundary(p.native.Snapshot(), false) {
			if p.at >= 0 && p.at < len(p.order) {
				p.loadSkipping(p.at, 0, stay, p.playing, p.Position())
			}
			return
		}
	}
	c.token, c.stream = 0, nil
	p.prepareNativeNext()
}
