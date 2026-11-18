package player

import (
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/audio"
	"mygo-music/internal/lyrics"
	"mygo-music/internal/music"
)

// Player is the existing main-thread API. Commands cross into the non-UI
// PlaybackController; getters read immutable confirmed snapshots. It owns no
// queue, output stream or natural-end logic. Configure callbacks on the UI thread.
type Player struct {
	Quiet            func(func())
	Source           func(string) music.Source
	Failed           func(string, error)
	Quality          func() string
	Normalize        func() bool
	Cache            *Cache
	Changed          func()
	post             func(func())
	controller       *PlaybackController
	posted           atomic.Bool
	changed, failure uint64 // UI thread only
	media            mediaSession
}

func New(post func(func())) *Player {
	p := &Player{post: post, controller: newPlaybackController()}
	// The initial idle snapshot needs no UI update (a window may not exist yet).
	select {
	case <-p.controller.wake:
	default:
	}
	go p.notifications()
	return p
}

// DownloadPriorityBusy observes playback and its in-flight reads without
// sending a command to the controller or changing the successor plan.
func (p *Player) DownloadPriorityBusy() bool {
	s := p.controller.latest.Load()
	if s.Playing || s.Loading || s.networkCurrent.inFlight() {
		return true
	}
	if s.netEaseAheadDone != nil {
		select {
		case <-s.netEaseAheadDone:
		default:
			return true
		}
	}
	if a := s.networkAhead; a != nil {
		select {
		case <-a.done:
			return a.dl.inFlight()
		default:
			return true
		}
	}
	return false
}

// notifications keeps at most one UI update outstanding. Even a UI that never
// executes that update cannot stop source completion, progress or natural next.
func (p *Player) notifications() {
	c := p.controller
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
			if p.post == nil || !p.posted.CompareAndSwap(false, true) {
				continue
			}
			p.post(func() {
				select {
				case <-c.done:
					p.posted.Store(false)
					return
				default:
				}
				seq := c.latest.Load().EventSeq
				p.apply()
				p.posted.Store(false)
				if c.latest.Load().EventSeq != seq {
					select {
					case c.wake <- struct{}{}:
					default:
					}
				}
			})
		}
	}
}

// apply is called only on the main thread. Controller workers never call app
// callbacks or the system's Now Playing implementation.
func (p *Player) apply() {
	s := p.controller.latest.Load()
	p.media.publish(s)
	if s.failureSeq != p.failure {
		p.failure = s.failureSeq
		if p.Failed != nil {
			p.Failed(s.failureSource, s.failureError)
		}
	}
	if s.ChangedSeq != p.changed {
		p.changed = s.ChangedSeq
		if p.Changed != nil {
			p.Changed()
		}
	}
}

// configure captures UI-owned settings and the source registry before a
// command. Source.Stream/Lyrics/RemoteState run on workers; natural next uses
// this frozen registry without consulting a UI map or waiting for post.
func (p *Player) configure(extra []music.Track) func(*playbackState) {
	sources := make(map[string]music.Source)
	add := func(tracks []music.Track) {
		for _, t := range tracks {
			if _, ok := sources[t.Source]; !ok && p.Source != nil {
				sources[t.Source] = p.Source(t.Source)
			}
		}
	}
	add(p.controller.latest.Load().Queue)
	add(extra)
	quality := music.Qualities[len(music.Qualities)/2]
	if p.Quality != nil {
		quality = p.Quality()
	}
	normalize := p.Normalize != nil && p.Normalize()
	cache := p.Cache
	return func(s *playbackState) {
		s.Source = func(id string) music.Source { return sources[id] }
		s.Quality = func() string { return quality }
		s.Normalize = func() bool { return normalize }
		s.Cache = cache
	}
}

func (p *Player) command(extra []music.Track, fn func(*playbackState)) {
	config := p.configure(extra)
	if p.controller.call(func(s *playbackState) { config(s); fn(s) }) {
		p.apply()
	}
}

func (p *Player) PlayQueue(tracks []music.Track, start int) {
	q := cloneTracks(tracks)
	p.command(q, func(s *playbackState) { s.PlayQueue(q, start) })
}
func (p *Player) PlayTrack(tracks []music.Track, i int) {
	q := cloneTracks(tracks)
	p.command(q, func(s *playbackState) { s.PlayTrack(q, i) })
}
func (p *Player) PlayShuffled(tracks []music.Track) {
	q := cloneTracks(tracks)
	p.command(q, func(s *playbackState) { s.PlayShuffled(q) })
}

// Feed runs off the UI thread and may answer asynchronously. Its callback only
// transfers owned track values to the controller; it must not mutate UI state.
func (p *Player) PlayRadio(name string, tracks []music.Track, feed func(func([]music.Track))) {
	q := cloneTracks(tracks)
	p.command(q, func(s *playbackState) { s.PlayRadio(name, q, feed) })
}
func (p *Player) AttachRadio(name string, feed func(func([]music.Track))) {
	p.command(nil, func(s *playbackState) { s.AttachRadio(name, feed) })
}
func (p *Player) EndRadio()              { p.command(nil, (*playbackState).EndRadio) }
func (p *Player) Next()                  { p.command(nil, (*playbackState).Next) }
func (p *Player) Prev()                  { p.command(nil, (*playbackState).Prev) }
func (p *Player) Jump(i int)             { p.command(nil, func(s *playbackState) { s.Jump(i) }) }
func (p *Player) TogglePlay()            { p.command(nil, (*playbackState).TogglePlay) }
func (p *Player) ToggleShuffle()         { p.command(nil, (*playbackState).ToggleShuffle) }
func (p *Player) CycleRepeat()           { p.command(nil, (*playbackState).CycleRepeat) }
func (p *Player) Seek(pos time.Duration) { p.command(nil, func(s *playbackState) { s.Seek(pos) }) }
func (p *Player) SetVolume(v float64)    { p.command(nil, func(s *playbackState) { s.SetVolume(v) }) }

// RefreshConfiguration captures changed UI preferences/source availability for
// future natural selections without asking the controller to read UI memory.
// An old quality/source prefetch is discarded; current playback stays in place.
func (p *Player) RefreshConfiguration() {
	p.command(nil, func(s *playbackState) {
		s.dropAhead()
		s.dropNetEaseAhead()
		s.refreshAhead()
	})
}

func (p *Player) Restore(session Session) {
	session.Queue, session.Order = cloneTracks(session.Queue), slices.Clone(session.Order)
	p.command(session.Queue, func(s *playbackState) { s.Restore(session) })
}
func (p *Player) Close() {
	p.SetVisualizationVisible(false)
	p.media.close()
	p.controller.Close()
}

// SetVisualizationVisible can be called without issuing a playback command.
func (p *Player) SetVisualizationVisible(visible bool) {
	p.controller.visual.SetEnabled(visible)
}
func (p *Player) Snapshot() PlaybackSnapshot { return p.controller.Snapshot() }
func (p *Player) Current() *music.Track      { return cloneTrack(p.controller.latest.Load().Current) }
func (p *Player) Queue() []music.Track       { return cloneTracks(p.controller.latest.Load().Queue) }
func (p *Player) PlayOrder() ([]int, int) {
	s := p.controller.latest.Load()
	return slices.Clone(s.Order), s.At
}
func (p *Player) Radio() string           { return p.controller.latest.Load().Radio }
func (p *Player) Shuffle() bool           { return p.controller.latest.Load().Shuffle }
func (p *Player) Repeat() RepeatMode      { return p.controller.latest.Load().Repeat }
func (p *Player) Playing() bool           { return p.controller.latest.Load().Playing }
func (p *Player) Loading() bool           { return p.controller.latest.Load().Loading }
func (p *Player) Ready() bool             { return p.controller.latest.Load().Ready }
func (p *Player) Err() string             { return p.controller.latest.Load().Error }
func (p *Player) Position() time.Duration { return p.controller.latest.Load().Position }
func (p *Player) Duration() time.Duration { return p.controller.latest.Load().Duration }
func (p *Player) Stream() *music.Stream   { return cloneStream(p.controller.latest.Load().Stream) }
func (p *Player) Lyrics() ([]lyrics.Line, bool) {
	s := p.controller.latest.Load()
	return cloneLyrics(s.Lyrics), s.LyricsLoaded
}
func (p *Player) Levels() ([Bands]float32, bool) {
	s := p.controller.latest.Load()
	if s.AudioOutput.Requested.Mode != OutputCompatible && s.AudioOutput.Native.Measurement.Token != p.controller.visual.Token() {
		return [Bands]float32{}, false
	}
	return s.Levels, s.HasLevels
}
func (p *Player) Downloaded() (float64, bool) {
	s := p.controller.latest.Load()
	return s.Downloaded, s.Downloading
}
func (p *Player) Session() (Session, bool) {
	s := p.controller.latest.Load()
	if s.Current == nil || s.At < 0 {
		return Session{}, false
	}
	return Session{Version: 1, Paused: !s.Playing, Queue: cloneTracks(s.Queue), Order: slices.Clone(s.Order), At: s.At, Shuffle: s.Shuffle, Repeat: s.Repeat, Position: s.Position.Milliseconds(), Radio: s.Radio}, true
}
func (p *Player) OpenAudio() (out Audio, err error) {
	var dl *download
	var duration int64
	p.controller.call(func(s *playbackState) {
		if s.stream != nil && s.track != nil {
			dl, _ = s.stream.source()
			duration = s.track.Duration
		}
	})
	if dl == nil {
		return nil, errors.New("player: nothing loaded")
	}
	return openShared(dl, duration)
}
func (p *Player) FindSong(src music.Source, t music.Track, quality string) (*Song, error) {
	return (&playbackState{Cache: p.Cache}).FindSong(src, t, quality)
}

// View fixtures still pass through the real ownership boundary.
func (p *Player) Cue(tracks []music.Track) {
	q := cloneTracks(tracks)
	p.command(q, func(s *playbackState) { s.Cue(q) })
}
func (p *Player) Hold(pos time.Duration)   { p.command(nil, func(s *playbackState) { s.Hold(pos) }) }
func (p *Player) Attach(out *audio.Player) { p.command(nil, func(s *playbackState) { s.Attach(out) }) }
func (p *Player) SetStream(st *music.Stream) {
	st = cloneStream(st)
	p.command(nil, func(s *playbackState) { s.SetStream(st) })
}
func (p *Player) SetLyrics(lines []lyrics.Line) {
	lines = cloneLyrics(lines)
	p.command(nil, func(s *playbackState) { s.SetLyrics(lines) })
}
