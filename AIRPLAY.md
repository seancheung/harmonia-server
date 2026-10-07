# Native AirPlay 1 / 2 sender

The initial implementation is an experimental, single-receiver sender written in
Go. It does not invoke or embed another AirPlay implementation. FFmpeg remains the
existing source decoder. The only additional dependency is Go's x/crypto for
ChaCha20-Poly1305; SRP, TLV8, binary plist, DNS-SD, RTSP and audio transport are
implemented locally.

## Data path and ownership

`Remote` owns queue/order, gain, selected output and the audible progress estimate.
One cancellable worker decodes a track into signed 16-bit stereo PCM at 44100 Hz.
The AirPlay session frames it as verbatim ALAC (352 samples/channel), encrypts
and paces RTP packets. It retains a bounded retransmission window. Audio nonces
use a 64-bit counter independent of the 16-bit RTP sequence to avoid nonce reuse.

The AirPlay 2 control flow is discovery → `/info` → HAP authentication → encrypted session
SETUP → reverse event channel → RECORD → optional SETPEERS → stream SETUP.
Receivers' returned audio and control ports are parsed by key. Server status does
not claim that hardware is audibly rendering: progress follows the negotiated
sender timeline. A receiver that accepts packets but stays silent needs hardware
investigation.

Pause/seek/next/output changes cancel the decoder and close transport resources.
The next worker waits for the previous transport lifetime to end before binding
clock ports. The queue is not mirrored into a remote service. Finish-track timers
suppress local automatic advancement without deleting queue entries. A restart
restores queue/position paused and disarms timers. Refresh outputs after restart.

## Initial boundaries

- IPv4 LAN DNS-SD for `_airplay._tcp` and `_raop._tcp`, including same-host
  receivers via multicast loopback. Duplicate device IDs are merged, preferring
  AirPlay 2. RAOP-only receivers use AirPlay 1. Unsupported advertised capabilities
  remain visible with an incompatibility reason.
- AirPlay 1 uses OPTIONS → ANNOUNCE (SDP) → SETUP (UDP) → RECORD. Supports
  plaintext ALAC or RSA-OAEP/SHA-1 wrapped AES-128-CBC, selected from TXT `et`.
  CBC resets per packet and leaves partial trailing blocks unencrypted. Session
  headers, receiver ports/latency, NTP, retransmissions, metadata, artwork and
  volume share the native sender. Volume queries omit RECORD and audio packets.
  Password/PIN, FairPlay-only and non-ANNOUNCE legacy receivers are not supported.
  No silent authentication/protocol downgrade after an AirPlay 2 failure.
- Single selected receiver. No multiroom, AWDL or IPv6-only discovery.
- HAP transient authentication, or explicit persistent PIN pair-setup and
  subsequent signed pair-verify. No legacy FairPlay/MFi compatibility route.
- Encrypted realtime type-96 audio, 44100 Hz/16-bit/stereo only. No buffered
  type-103 transport, receiver-clock following or sample-accurate gapless.
- NTP responder or sender-master unicast PTP (capability bit 41). PTP includes
  Sync/Follow_Up, Announce, delay responses and unicast grants. It is not a general
  BMCA clock stack. PTP requires UDP 319/320. If the OS denies permission to
  bind these ports, the session attempts AirPlay 2 NTP timing on a negotiated
  unprivileged port. Receivers that reject NTP still require PTP port access;
  setup reports both failures. Other bind errors and synchronization failures
  remain errors. This does not switch to original AirPlay.
- Playback waits for receiver timing traffic in both NTP and PTP modes. NTP
  sync packets advance the transmission head and its clock mapping together;
  progress remains an estimate, not a receiver rendering acknowledgement.
- DMAP music item metadata (title, artist, album, album artist, stable track ID,
  track/disc number and duration), initial RTP progress including seeks, and
  volume. JPEG/PNG covers (up to 8 MiB) are sent on each playback session,
  including track changes and resumed connections; missing/invalid covers send
  an image/none clear request. Artwork rejection is logged without aborting
  audio. Receiver UI support varies; MediaRemote artwork/UI, remote-button
  integration, Home stereo groups and automatic reconnect are not implemented.
- Authentication/decoder/transport failures remain explicit; no fake devices,
  successful pairing or fabricated receiver status.

## Validation

Run `go test -race ./...` and `go vet ./...`. Protocol tests cover binary plist,
TLV fragmentation, independent SRP server equations, authenticated frames, audio
sample packing, sequence wrap, malformed messages, and a loopback receiver that
performs the transient handshake, event acknowledgement, timing and retransmit.
The loopback receiver is a protocol fixture, not evidence of hardware support.
Fuzz targets cover binary plist and DNS message parsing.

Before treating this sender as production-ready, run it on the receiver LAN:

1. Discover the output and verify its advertised address, port and capabilities.
2. Test both transient and PIN-controlled sessions; restart and verify stored
   pairing reuse without displaying another PIN.
3. Play for at least 30 minutes (crossing RTP sequence wrap); inspect errors,
   network traffic and audible continuity, including receiver clock traffic.
4. Exercise pause/resume, seek, next/previous, volume/mute, queue edits, sleep
   timer, page closure and server restart.
5. Interrupt Wi-Fi and reboot the receiver. Verify error reporting and successful
   manual reconnection, without leaked sockets/processes or stuck clock ports.
6. Test a PTP-capable receiver separately from NTP; measure initial synchronization
   and progress offset. A successful SETUP is not sufficient acceptance evidence.

AirPlay 1 RAOP has loopback coverage for discovery/merging, SDP, Session headers,
UDP audio, NTP, metadata, artwork clearing, volume without RECORD, and AES-CBC.
The locally discovered AirPlayer advertises RAOP but rejects ANNOUNCE with 501.
A diagnostic plist SETUP returned zero audio ports; this alternate route is not
enabled. Audible playback on that receiver remains unverified.

## Protocol references

These references inform wire formats; their AirPlay code is not linked or vendored.

- https://pyatv.dev/documentation/protocols/
- https://www.rfc-editor.org/rfc/rfc5054 (SRP group)
- https://github.com/akustikrausch/airplay2-sender-cpp (handshake/event ordering)
- https://github.com/music-assistant/airplay-cli/blob/main/DESIGN.md (transport/timing)
- https://github.com/mikebrady/shairport-sync (receiver packet interpretation)
