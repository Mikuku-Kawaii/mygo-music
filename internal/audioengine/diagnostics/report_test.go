package diagnostics

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
)

// Diagnostics preserve counters and typed device failure data without
// serializing track identity, paths, endpoints or raw provider exceptions.
func TestDiagnosticReportRedactsRawPlaybackData(t *testing.T) {
	const secret = "secret-user-data"
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 32}
	n := controller.Snapshot{
		State: controller.Failed, ErrorKind: "device-lost", ErrorCode: 0x88890004, Error: "https://host/path?token=" + secret,
		Current:     controller.Item{Source: secret, ID: secret, Title: secret},
		Source:      decode.Info{Container: secret, Format: f},
		Output:      output.Opened{EndpointID: secret, Format: f, Period: 10 * time.Millisecond},
		OutputStats: output.Snapshot{Epoch: 6, Underruns: 2, UnderrunFrames: 60}, Reconfigure: secret,
	}
	r := Capture(Playback{Mode: "wasapi-exclusive", Endpoint: secret, Failed: true, Native: n})
	data, err := json.Marshal(r)
	if err != nil || bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte("https://")) {
		t.Fatal("export leaked playback data or failed", err)
	}
	if r.Failure == nil || r.Failure.HRESULT != "0x88890004" || r.Native == nil || r.Native.DeviceCounters.UnderrunFrames != 60 ||
		r.Native.Live || r.EffectiveMode != "" || r.Runtime.HeapBytes == 0 {
		t.Fatal("missing or misleading typed evidence", r)
	}
	remote := Capture(Playback{Remote: true, Ready: true, Playing: true, Native: n})
	if remote.EffectiveMode != "remote" || remote.Native != nil {
		t.Fatal("remote playback received local PCM claims")
	}
}
