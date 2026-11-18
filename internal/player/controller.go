package player

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/audio"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/lyrics"
	"mygo-music/internal/music"
)

// PlaybackSnapshot is a copy of confirmed controller state. The compatibility
// backend retains MyGo's position semantics; it is not native PCM certification.
type PlaybackSnapshot struct {
	EventSeq, SessionGeneration, ChangedSeq, ArtworkSeq uint64
	QueueRevision                                       uint64
	Backend                                             string
	AudioOutput                                         AudioOutput
	Current                                             *music.Track
	Queue                                               []music.Track
	Order                                               []int
	At                                                  int
	Shuffle                                             bool
	Repeat                                              RepeatMode
	Radio                                               string
	Playing, Loading, Ready                             bool
	Error                                               string
	Position, Duration                                  time.Duration
	Stream                                              *music.Stream
	Lyrics                                              []lyrics.Line
	LyricsLoaded                                        bool
	Downloaded                                          float64
	Downloading                                         bool
	Levels                                              [Bands]float32
	HasLevels                                           bool
	Artwork                                             []byte
	Closed                                              bool
	failureSeq                                          uint64
	failureSource                                       string
	failureError                                        error
	networkAhead                                        *ahead
	netEaseAheadDone                                    <-chan struct{}
	networkCurrent                                      *download
}

type controlCall struct {
	fn   func(*playbackState)
	done chan struct{}
}

// PlaybackController owns the application's authoritative session. Only its
// loop accesses state. Decoder/source results and end callbacks enter events;
// UI dispatch is a separate, coalesced notification path, never an audio gate.
type PlaybackController struct {
	history             chan func()
	historyDone         chan struct{}
	state               *playbackState
	calls               chan controlCall
	events              chan func()
	loads               chan func()
	media               chan mediaRequest
	stop, done          chan struct{}
	wake                chan struct{}
	latest              atomic.Pointer[PlaybackSnapshot]
	admit               sync.Mutex
	once                sync.Once
	retired             sync.WaitGroup
	changed, artworkSeq uint64
	artwork             []byte
	failureSeq          uint64
	failureSource       string
	failureError        error
	visual              analysis.Subscription // shared with the active transport through atomic tokens
}

func newPlaybackController() *PlaybackController {
	c := &PlaybackController{calls: make(chan controlCall, 32), events: make(chan func(), 64), loads: make(chan func(), 32), media: make(chan mediaRequest, 16), stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1)}
	c.state = newPlaybackState(func(fn func()) { c.deliver(fn) })
	c.state.visual = &c.visual
	c.history = make(chan func(), 32)
	c.historyDone = make(chan struct{})
	c.state.complete = func(fn func()) { c.history <- fn }
	go func() {
		defer close(c.historyDone)
		for fn := range c.history {
			fn()
		}
	}()
	c.state.outputGate = make(chan struct{}, 1)
	c.state.outputGate <- struct{}{}
	c.state.compatibleIdleWait = 5500 * time.Millisecond
	c.state.deliver = c.deliver
	c.state.Quiet = c.state.post
	c.state.after = func(d time.Duration, fn func()) {
		time.AfterFunc(d, func() {
			select {
			case <-c.stop:
				return
			default:
				fn()
			}
		})
	}
	c.state.onSync = func() { c.changed++ }
	c.state.onRedraw = func() {}
	c.state.retire = func(out *audio.Player) <-chan struct{} {
		c.retired.Add(1)
		done := make(chan struct{})
		time.AfterFunc(100*time.Millisecond, func() { defer c.retired.Done(); defer close(done); out.Close() })
		return done
	}
	c.state.Failed = func(source string, err error) {
		c.failureSeq++
		c.failureSource, c.failureError = source, err
	}
	c.state.onAnnounce = func(_ *music.Track, art []byte) {
		c.artworkSeq++
		c.artwork = slices.Clone(art)
	}
	c.state.runLoad = func(fn func()) {
		// At most two source opens run concurrently. Canceled queued jobs are
		// cheap to retire, including closing any prefetch resource they own.
		select {
		case c.loads <- fn:
		default:
			// Admission is serialized on this loop; retire the oldest canceled
			// job's cancellation before admitting the latest selection. Canceled
			// work returns before querying a source or opening a decoder.
			select {
			case old := <-c.loads:
				old()
			default:
			}
			c.loads <- fn
		}
	}
	c.publish(true)
	for range 2 {
		go c.loadWorker()
	}
	go c.run()
	return c
}

func (c *PlaybackController) loadWorker() {
	for {
		select {
		case <-c.stop:
			return
		case fn := <-c.loads:
			fn()
		}
	}
}

// deliver and shutdown serialize admission so a successful handoff is always
// consumed, even when Close races with a source returning an open stream.
func (c *PlaybackController) deliver(fn func()) bool {
	c.admit.Lock()
	defer c.admit.Unlock()
	select {
	case <-c.stop:
		return false
	default:
	}
	c.events <- fn
	return true
}

func (c *PlaybackController) call(fn func(*playbackState)) bool {
	call := controlCall{fn: fn, done: make(chan struct{})}
	select {
	case <-c.stop:
		return false
	default:
	}
	select {
	case c.calls <- call:
	case <-c.stop:
		return false
	}
	select {
	case <-call.done:
		return true
	case <-c.done:
		return false
	}
}

func (c *PlaybackController) Close() {
	c.once.Do(func() { c.admit.Lock(); close(c.stop); c.admit.Unlock() })
	<-c.done
}

func (c *PlaybackController) run() {
	defer close(c.done)
	defer func() { close(c.history); <-c.historyDone }()
	tick := time.NewTimer(time.Hour)
	defer tick.Stop()
	tick.Stop()
	var clock <-chan time.Time
	for {
		var call *controlCall
		var nativeChanges <-chan struct{}
		if c.state.native != nil {
			nativeChanges = c.state.native.Changes()
		}
		refresh := true
		select {
		case <-c.stop:
			p := c.state
			p.gen++
			p.cmds.close()
			p.endRadio()
			if p.loadCancel != nil {
				p.loadCancel()
			}
			p.dropAhead()
			// Invalidate first; already accepted load results now only release
			// their streams. Remote polls and fades also fail their identity check.
			p.stop()
			if p.outputReleased != nil {
				<-p.outputReleased
			}
			c.retired.Wait()
			for {
				select {
				case fn := <-c.events:
					fn()
				default:
					goto drained
				}
			}
		drained:
			for {
				select {
				case fn := <-c.loads:
					fn()
				default:
					goto closed
				}
			}
		closed:
			// A load worker may already own a device lease when Close
			// cancels it. Wait for that lease, even if its handle was
			// never installed on playbackState.
			<-p.outputGate
			p.playing, p.loading = false, false
			c.publish(true)
			s := *c.latest.Load()
			s.Closed = true
			c.latest.Store(&s)
			return
		case item := <-c.calls:
			call = &item
			item.fn(c.state)
		case request := <-c.media:
			request.apply(c.state)
		case fn := <-c.events:
			fn()
		case <-nativeChanges:
			c.state.observeNative()
		case <-clock:
			refresh = c.state.pollEnd()
		}
		previousSeq := c.latest.Load().EventSeq
		c.publish(refresh)
		if call != nil {
			close(call.done)
		} else if c.latest.Load().EventSeq != previousSeq {
			select {
			case c.wake <- struct{}{}:
			default:
			}
		}
		tick.Stop()
		clock = nil
		if c.state.playing && !c.state.loading && (c.state.out != nil || c.state.remote != nil) {
			tick.Reset(25 * time.Millisecond)
			clock = tick.C
		}
	}
}

func (c *PlaybackController) publish(refresh bool) {
	p := c.state
	var s PlaybackSnapshot
	previous := c.latest.Load()
	if previous != nil {
		s = *previous
	}
	if refresh {
		s.Backend = "mygo-compatible"
		if p.output.Mode != OutputCompatible {
			s.Backend = string(p.output.Mode)
		}
		s.AudioOutput = AudioOutput{Requested: p.output, Native: p.nativeState, Remote: p.remote != nil, InterruptedPlaying: p.interruptedPlaying}
		s.SessionGeneration = uint64(p.gen)
		s.QueueRevision = p.queueRevision
		s.ChangedSeq, s.ArtworkSeq = c.changed, c.artworkSeq
		s.Current = cloneTrack(p.track)
		if previous == nil || previous.QueueRevision != p.queueRevision {
			s.Queue, s.Order = cloneTracks(p.queue), slices.Clone(p.order)
		}
		s.At = p.at
		s.Shuffle, s.Repeat, s.Radio = p.shuffle, p.repeat, p.radio
		s.Error = p.err
		s.failureSeq, s.failureSource, s.failureError = c.failureSeq, c.failureSource, c.failureError
		s.Stream = cloneStream(p.info)
		if s.Stream != nil {
			s.Stream.URL, s.Stream.Headers = "", nil
		}
		// The controller replaces these lyrics as a whole. Callers get copies through the facade.
		s.Lyrics, s.LyricsLoaded = p.lyrics, p.lyricsLoaded
		s.Artwork = c.artwork // replaced, never mutated
		s.networkAhead = p.ahead
		s.netEaseAheadDone = nil
		if p.neteaseAhead != nil {
			s.netEaseAheadDone = p.neteaseAhead.done
		}
		s.networkCurrent = nil
		if p.stream != nil {
			s.networkCurrent, _ = p.stream.source()
		}
	}
	s.Playing, s.Loading, s.Ready = p.Playing(), p.Loading(), p.Ready()
	s.Position, s.Duration = p.Position(), p.Duration()
	s.Downloaded, s.Downloading = p.Downloaded()
	s.Levels, s.HasLevels = p.Levels()
	if !refresh && previous != nil && s.Position == previous.Position && s.Loading == previous.Loading && s.Downloaded == previous.Downloaded && s.Levels == previous.Levels {
		return
	}
	s.EventSeq++
	c.latest.Store(&s)
}

// Snapshot returns caller-owned copies of every mutable collection.
func (c *PlaybackController) Snapshot() PlaybackSnapshot {
	s := *c.latest.Load()
	s.Current, s.Queue, s.Order = cloneTrack(s.Current), cloneTracks(s.Queue), slices.Clone(s.Order)
	s.Stream, s.Lyrics, s.Artwork = cloneStream(s.Stream), cloneLyrics(s.Lyrics), slices.Clone(s.Artwork)
	return s
}

func cloneTrack(t *music.Track) *music.Track {
	if t == nil {
		return nil
	}
	v := *t
	v.Artists, v.Album.Artists = slices.Clone(t.Artists), slices.Clone(t.Album.Artists)
	return &v
}
func cloneTracks(ts []music.Track) []music.Track {
	out := slices.Clone(ts)
	for i := range out {
		out[i] = *cloneTrack(&ts[i])
	}
	return out
}
func cloneStream(st *music.Stream) *music.Stream {
	if st == nil {
		return nil
	}
	out := *st
	out.Headers = maps.Clone(st.Headers)
	return &out
}
func cloneLyrics(lines []lyrics.Line) []lyrics.Line {
	out := slices.Clone(lines)
	for i := range out {
		out[i].Words = slices.Clone(lines[i].Words)
	}
	return out
}
