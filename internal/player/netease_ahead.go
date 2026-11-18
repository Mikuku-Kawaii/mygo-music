package player

import (
	"context"

	"mygo-music/internal/music"
)

// neteaseAhead holds one prepared file for the next track in native mode.
// Its worker writes the result before closing done. The controller either
// hands the job to a foreground load or discards it, transferring the lease.
type neteaseAhead struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	file   *neteaseFLAC
	info   *music.Stream
}

func (p *playbackState) getNetEaseAhead() {
	t, ok := p.upNext()
	if !ok || t.Source != neteaseSource || t.Unavailable {
		p.dropNetEaseAhead()
		return
	}
	src := p.sourceOf(&t)
	if src == nil || src.Info().Playback != music.PlaybackStream {
		p.dropNetEaseAhead()
		return
	}
	if p.neteaseAhead != nil && p.neteaseAhead.id == t.ID {
		return
	}
	p.dropNetEaseAhead()
	ctx, cancel := context.WithCancel(context.Background())
	a := &neteaseAhead{id: t.ID, cancel: cancel, done: make(chan struct{})}
	p.neteaseAhead = a
	if p.neteaseAheadGate == nil {
		p.neteaseAheadGate = make(chan struct{}, 1)
	}
	cache, gate := p.Cache, p.neteaseAheadGate
	go func() {
		defer close(a.done)
		// A plugin Stream call may be unable to observe cancellation until it
		// returns. Keep superseded jobs from piling up at the source.
		select {
		case <-ctx.Done():
			return
		case gate <- struct{}{}:
		}
		defer func() { <-gate }()
		a.file, a.info, _ = cache.openNeteaseFLAC(ctx, src, t.ID)
	}()
}

func (p *playbackState) takeNetEaseAhead(t music.Track, src music.Source) *neteaseAhead {
	a := p.neteaseAhead
	if a == nil || t.Source != neteaseSource || t.ID != a.id || src == nil || src.Info().Playback != music.PlaybackStream {
		return nil
	}
	p.neteaseAhead = nil
	return a
}

func (p *playbackState) dropNetEaseAhead() {
	if a := p.neteaseAhead; a != nil {
		p.neteaseAhead = nil
		a.discard()
	}
}

func (a *neteaseAhead) discard() {
	a.cancel()
	go func() {
		<-a.done
		a.file.release()
	}()
}

func (a *neteaseAhead) take(ctx context.Context) (*neteaseFLAC, *music.Stream) {
	if a == nil {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		a.discard()
		return nil, nil
	case <-a.done:
		a.cancel()
		return a.file, a.info
	}
}
