package player

import (
	"time"

	"github.com/egoist/mygo/audio"

	"mygo-music/internal/lyrics"
	"mygo-music/internal/music"
)

// These set a player up as it would be after loading, without loading
// anything, for the tests of the views that show it.

// Cue makes tracks the queue, in order, with the first current.
func (p *playbackState) Cue(tracks []music.Track) {
	if len(tracks) == 0 {
		return
	}
	p.queue = tracks
	p.order = make([]int, len(tracks))
	for i := range p.order {
		p.order[i] = i
	}
	p.at = 0
	p.track = &p.queue[0]
	p.queueChanged()
}

// SetLyrics gives the current track lines, as loaded.
func (p *playbackState) SetLyrics(lines []lyrics.Line) {
	p.lyrics, p.lyricsLoaded = lines, true
}

// Hold makes the player play at pos with nothing to play: its position
// stays there.
func (p *playbackState) Hold(pos time.Duration) {
	p.pending = pos
	p.playing = true
}

// Attach makes out the current track's audio, playing.
func (p *playbackState) Attach(out *audio.Player) {
	p.out = out
	p.playing = true
}

// SetStream makes st what the current track streams.
func (p *playbackState) SetStream(st *music.Stream) { p.info = st }
