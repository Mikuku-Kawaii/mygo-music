# Native audio laboratory and EngineHandle

The application still uses its upstream player. This package and
`cmd/audiolab` implement PP-M0-02 through PP-M0-09 and the PP-M1-09 backend
EngineHandle independently of MyGo UI. Application PlaybackController migration,
UI integration, shared output, device recovery, DSP and a production gapless
queue remain separate tasks.

```powershell
go test ./internal/audioengine/...
go run ./cmd/audiolab -mode verify
go run ./cmd/audiolab -mode fixtures -dir ./build/pcm-fixtures
go run ./cmd/audiolab -mode probe
go run ./cmd/audiolab -mode smoke -duration 3s
go run ./cmd/audiolab -mode handle-smoke -container 16
go run ./cmd/audiolab -mode smoke -rate 192000 -valid 32 -duration 60s -gc-stress
go run ./cmd/audiolab -mode play -file 'D:/Music/song.wav' -container 32
```

`verify` and normal tests do not open hardware. `probe` queries active Windows
render endpoints, without opening a stream. `smoke` opens exclusive output,
submits silence, pauses/resumes, drains, resets to a new epoch and drains again.
It may temporarily interrupt other applications using that endpoint. `play`
submits the named file at unity gain and optionally concatenates `-next` with
the same native format. It has no software volume or automatic fallback.
Generated full-scale edge vectors are **offline-only**, not listening material.
The separately named continuous triangle fixtures peak at 1/128 full scale.

Output is JSON, exit 0 means the requested operation passed. Device query
support, successful Initialize, successful Start, clean application supply and
external sample equality are distinct facts. The probe matrix is finite and is
not a complete device capability list. Empty `-device` resolves the console
default once at open; a nonempty ID selects exactly that endpoint. No endpoint
is reopened or silently replaced after an error.

## Supported M0 formats

- RIFF WAV signed integer PCM: 16, 24, 32-bit containers, including extensible
  valid bits and channel masks. Native mono/stereo decode; stereo output only.
- FLAC: existing `github.com/mewkiz/flac v1.0.14`, integer subframes, 16/24-bit
  stereo verified at 44.1/48/96/192 kHz. Header-inherited rate/precision are
  resolved before subframe decode. 32-bit FLAC is explicitly rejected.
- WAV seeks are exact direct frame offsets. FLAC seeks are exact cancellable
  forward scans; backward seeks reopen and scan from the start. They are not
  claimed to be indexed/constant-time. A canceled/failed seek may have advanced
  the decoder; its owner must discard/reseek before publishing a new generation.
- RF64, floating WAV, multichannel output and other codecs are unavailable in
  this native path; they never fall through to the upstream float32 mixer.
- Output is little-endian, left-aligned in its container. Internal integer
  samples are signed right-aligned int32. There is no rounding, gain or dither.

## PP-M2 live playback

The application's PlaybackController supplies one transport item and a
replaceable `render.NextSlot`. Render can commit a ready compatible decoder
at a source boundary. The transport publishes that boundary only after the
device clock presents the successor, and the application then acknowledges
the plan. Queue order, repeats and completion history stay with the
application controller.

The processed exclusive mode starts with the native PCM decoder where
available, and uses pinned MyGo codec adapters for supported local compressed
formats. `dsp` prepares a stereo float64 processor, then writes quantized
32-bit PCM through the same output ring. Source-rate and fixed-rate
continuation, trim and configuration behavior are documented in
[`M2_AUDIO_REPORT.md`](../../../docs/M2_AUDIO_REPORT.md).

## PP-M1-09 standalone EngineHandle baseline

`controller.New(Config)` creates an idle handle without opening hardware.
`SetQueue` copies local Media entries (Source/ID/Title/Path) and prepares the
selected entry **paused**. `Select` explicitly opens a new session, preserving
the latest pause/play intent; `Play`, `Pause`, `Stop` and `Cancel` are asynchronous.
Stop releases the session but retains the queue/selection. End keeps the current
track; Select is required to restart it. There is no automatic queue advance yet.

A returned Receipt confirms admission only. Wait returns an applied, failed,
rejected, superseded or canceled result and can be repeated/concurrent. Method
contexts bound admission, Wait contexts bound waiting; use Cancel(receipt.ID())
to cancel a pending operation. Cancel's own receipt confirms release; a newer
command may supersede it. Canceling an already-completed receipt is rejected.

One control goroutine owns intent, selection and queue revision. One transport
worker serializes open/control/close; old preparation results are discarded and
closed before a new device opens. Command capacity is 32 (overflow is ErrBusy),
pending work is coalesced to one job, and reports have eight slots. These queues
never sit in Render/Output. Slow local kernel/device calls may delay preparation
and Close; contexts cannot forcibly interrupt them. Close joins owned workers,
releases the device/decoder, and settles every accepted receipt.

Snapshot returns values only; Queue returns a copy of public identities, without
local paths or engine objects. Requested/applied commands and generations,
selected/current item, and desired/effective playback are distinct. Seek remains
zero and config revision stays one in this scope. Changes is a one-slot wake-up
for one consumer; read Snapshot after waking. It is not an event log, and leaving
it unread cannot stall playback. Device observations are sampled at 10 Hz; only
changed values are published. Position uses device presentation bounded to content
frames, not decoder read-ahead or a UI timer. PositionExact becomes false after
underrun/inaccurate clock evidence; it does not certify physical DAC timing.

Config fixes the endpoint, period and integer output container for the handle.
ContainerBits=0 retains the decoder container (FLAC uses int32); a device may
reject e.g. 16 valid bits in 32 container bits. An explicitly supported container
can repack without changing effective bits; there is no precision downgrade or
implicit fallback. Current sources are local WAV/FLAC only. No existing player,
UI, music.Source resolver, plugin, storage, Seek or repeat/shuffle is connected.

`handle-smoke` generates one-second silent WAV and FLAC files, drives the public
facade through prepare/play/pause/resume/manual select/presented end/stop/close,
and emits snapshots as JSON. Use `-container 16` for the generated 16-bit samples;
`-device` and `-period` select the endpoint/period. Other smoke flags do not apply.
See `docs/M1_ENGINE_HANDLE_REPORT.md` and `verification/2026-11-17/m1/` in the
parent project for software, device and remaining race-check evidence.

## Ownership and resources

Each Decoder belongs to one worker; Read returns **frames**. EOF, temporary
unavailability, cancellation and failure are distinct. Local file I/O checks
cancellation at calls/frame boundaries, not by preempting a kernel disk read.

`buffer.Blocks` has one producer and one consumer. Acquire/Publish transfers a
slot; Peek/Release returns it. Published sample storage is read-only until
release. Current and next decoders each have at most 32 MiB of preallocated
PCM (two seconds where possible); combined cap is 64 MiB. FLAC decoder scratch
and compressed metadata are separate from the PCM budget. The FLAC library
allocates per compressed frame, on decoder workers only.

One Render goroutine joins the blocks and writes `buffer.Ring`; one Output
goroutine reads it. The native PCM path uses 80 ms, the processed path 120 ms,
and both respect a 4 MiB output-ring cap.
An actual device packet larger than the ring is rejected. Packing and copying
do not allocate. Idle/full queues use cancellable 1 ms pacing outside the
packet work. No application callbacks, file I/O or ordinary mutex waits occur
on the output worker. Caller-side command serialization has its own mutex.

The Windows amd64 backend locks its goroutine to an OS thread, initializes and
releases COM/MMCSS there, and waits on separate audio and control events.
Buffer-alignment retry reactivates the client once. It prefills an entire
exclusive packet before Start; effective Playing is only published on success.
Clock frequency is read from IAudioClock; snapshots distinguish submitted,
content-submitted and presented frames. Presentation refers to the driver
endpoint, not the DAC's internal conversion. Pause retains buffered samples;
Reset accepts a new compatible ring/newer epoch and stays paused. The old
producer must be canceled by its owner. Drain waits for finite producer EOF and
the last content frame to be presented; a paused stream must be resumed first.

Missing live data produces legal PCM silence and increments underruns; the run
loses continuity evidence. End padding required by full device packets is
counted separately and never inserted between the two prepared sources.
Clock/device failure ends the worker with an error; it does not simulate time
or fall back to another backend. Close cancels/join semantics are explicit;
callers must Close even after device failure to release the control wake event.
Output telemetry counters are individually atomic diagnostic observations.
EngineHandle assembles immutable snapshots from them; its publication is atomic,
but individual device counters may have been sampled across a packet boundary.

## Sources and licenses

- Application baseline: `egoist/mygo-music@298c88a03407844ac1c4eb76e3590edfe4577121`,
  AGPL-3.0 as retained at repository root. The Windows test URL fix uses the
  existing `music.FileURL`; production player behavior is unchanged.
- Windows COM ABI/helper patterns were adapted with reference to MyGo v0.9.0
  `audio/output_windows.go` (MIT, retained in `LICENSE.mygo`). Exclusive events,
  format probing, clock, thread lifetime, queues and tests are implemented here;
  none of its shared 48 kHz mixing/conversion is reused.
- `github.com/mewkiz/flac v1.0.14`: Unlicense, already locked in go.sum;
  promoted to direct dependency. Decoder and fixture encoder only, no copied
  source. `golang.org/x/sys v0.48.0`: BSD-3-Clause, existing pinned dependency
  promoted to direct for Windows APIs. No module-cache edits or version upgrades.
- No ASMRplayer source, credentials, UI or Rust/native DSP was imported.

The dated report and acceptance limits live in the parent project's
`docs/M0_AUDIO_REPORT.md` and `verification/2026-11-17/m0/`.
