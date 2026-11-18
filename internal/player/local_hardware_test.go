package player

import (
	"encoding/json"
	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/local"
	"mygo-music/internal/music"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A real scanned local playlist survives seek/save/restore and advances at
// presentation, including while a rescan runs and no UI posts are consumed.
func TestM1LocalHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent local WASAPI integration")
	}
	for _, mode := range []OutputMode{OutputShared, OutputExclusive} {
		t.Run(string(mode), func(t *testing.T) {
			dir, data := t.TempDir(), t.TempDir()
			f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
			for _, ext := range []string{"wav", "flac"} {
				path := filepath.Join(dir, "测试."+ext)
				var err error
				if ext == "wav" {
					err = fixture.WriteWAV(path, f, make([]int32, 96000))
				} else {
					err = fixture.WriteFLAC(path, f, make([]int32, 96000))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			src := local.New(data)
			src.AddFolders(dir)
			deadline := time.Now().Add(5 * time.Second)
			for src.Scanning() {
				if time.Now().After(deadline) {
					t.Fatal("scan timeout")
				}
				time.Sleep(time.Millisecond)
			}
			result, err := src.Search("测试", music.SearchTracks, 0)
			if err != nil || len(result.Tracks) != 2 {
				t.Fatal(result, err)
			}
			list, err := src.CreatePlaylist("M1 integration")
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{result.Tracks[0].ID, result.Tracks[1].ID}
			if err = src.AddToPlaylist(list.ID, ids); err != nil {
				t.Fatal(err)
			}
			if err = src.MovePlaylistTrack(list.ID, 0, 1); err != nil {
				t.Fatal(err)
			}
			playlist, err := src.Playlist(list.ID)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := controlledPlayer(t, src)
			if err = p.SetOutput(OutputPreferences{Mode: mode}); err != nil {
				t.Fatal(err)
			}
			p.PlayQueue(playlist.Tracks, 0)
			p.TogglePlay()
			awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && !s.Playing })
			p.Seek(250 * time.Millisecond)
			awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.AppliedGeneration.Seek == 1 })
			saved, ok := p.Session()
			if !ok || saved.Position != 250 || !saved.Paused {
				t.Fatal(saved)
			}
			p.Close()
			restored, _ := controlledPlayer(t, src)
			if err = restored.SetOutput(OutputPreferences{Mode: mode}); err != nil {
				t.Fatal(err)
			}
			restored.Restore(saved)
			s := awaitPlayback(t, restored, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.AppliedGeneration.Seek == 1 })
			if s.Playing || s.Position != 250*time.Millisecond || s.Current.ID != playlist.Tracks[0].ID {
				t.Fatal(s)
			}
			restored.SetVolume(.4)
			restored.TogglePlay()
			src.Rescan()
			awaitPlayback(t, restored, func(s PlaybackSnapshot) bool {
				return s.Current.ID == playlist.Tracks[1].ID && s.AudioOutput.Native.EffectivePlaying
			})
			s = awaitPlayback(t, restored, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.State == engine.Ended && !s.Playing })
			if s.Position != time.Second || s.AudioOutput.Native.Underruns != 0 {
				t.Fatal(s)
			}
			restored.Close()
			completions := src.Completions()
			if len(completions) != 2 || completions[0].ID != playlist.Tracks[1].ID || completions[1].ID != playlist.Tracks[0].ID || completions[0].Position != 1000 {
				t.Fatal(completions)
			}
			reloaded := local.New(data)
			if len(reloaded.Completions()) != 2 {
				t.Fatal("history not persisted")
			}
			b, _ := json.Marshal(s.AudioOutput.Native)
			t.Log(string(b))
		})
	}
}
