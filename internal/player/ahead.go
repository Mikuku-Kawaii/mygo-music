package player

import "mygo-music/internal/music"

// The next track's audio is asked for once the current one plays, and comes in
// meanwhile, so that it starts at once when its turn comes: a source may take
// seconds to give a stream, as one that works it out with programs does.

// ahead is a track's audio asked for before its turn.
type ahead struct {
	source, id, quality string
	queueRevision       uint64
	done                chan struct{} // closed once info, dl and err are in
	info                *music.Stream
	dl                  *download // nil for a song on disk, which opens at once
	err                 error
}

func (a *ahead) is(t music.Track, quality string) bool {
	return a.source == t.Source && a.id == t.ID && a.quality == quality
}

// upNext is the track that plays when the current one ends, if one does by
// itself: not a radio's next, which its feed may not have given yet.
func (p *playbackState) upNext() (music.Track, bool) {
	if p.at < 0 || p.at >= len(p.order) || p.repeat == RepeatOne {
		return music.Track{}, false
	}
	at := p.at + 1
	if at >= len(p.order) {
		if p.repeat != RepeatAll || p.feed != nil {
			return music.Track{}, false
		}
		at = 0
	}
	if at == p.at {
		return music.Track{}, false
	}
	return p.queue[p.order[at]], true
}

// getAhead asks for the next track's audio at quality, unless it's kept
// already or its source plays it elsewhere. What was asked for before that
// isn't next any more is let go.
func (p *playbackState) getAhead(quality string) {
	t, ok := p.upNext()
	if a := p.ahead; a != nil {
		if ok && a.queueRevision == p.queueRevision && a.is(t, quality) {
			return
		}
		p.dropAhead()
	}
	if !ok {
		p.dropNetEaseAhead()
		return
	}
	if t.Source == neteaseSource {
		p.getNetEaseAhead()
		return
	}
	p.dropNetEaseAhead()
	src := p.sourceOf(&t)
	if src == nil || src.Info().Playback == music.PlaybackRemote {
		return
	}
	cache := p.Cache
	a := &ahead{source: t.Source, id: t.ID, quality: quality, queueRevision: p.queueRevision, done: make(chan struct{})}
	p.ahead = a
	go func() {
		defer close(a.done)
		if dl, info, ok := cache.lookup(t.Source, t.ID, quality); ok {
			a.dl, a.info = dl, info
			return
		}
		info, err := src.Stream(t.ID, quality)
		if err == nil && info == nil {
			err = music.ErrUnavailable
		}
		if err != nil {
			a.err = err
			return
		}
		a.info = info
		if _, isFile := music.PathOf(info.URL); !isFile {
			a.dl = cache.download(t.Source, t.ID, quality, info)
		}
	}()
}

// takeAhead returns what was asked for ahead of t at quality, if it was, for
// the caller to open or close.
func (p *playbackState) takeAhead(t music.Track, quality string) *ahead {
	a := p.ahead
	if a == nil || a.queueRevision != p.queueRevision || !a.is(t, quality) {
		return nil
	}
	p.ahead = nil
	return a
}

// dropAhead lets go of what was asked for ahead, stopping it if it's still
// coming in. What has all come in stays in the cache.
func (p *playbackState) dropAhead() {
	a := p.ahead
	p.ahead = nil
	if a == nil {
		return
	}
	go a.close()
}

// close stops what was asked for ahead once it's in, if it's still coming in.
func (a *ahead) close() {
	<-a.done
	if a.dl != nil {
		a.dl.Close()
	}
}
