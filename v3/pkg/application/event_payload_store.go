package application

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// Oversized Go→JS events are not spliced into the JavaScript source passed to
// the webview's eval. Above a platform-specific size WebKit switches from inline
// IPC message data to an out-of-line shared-memory transfer, and one of the two
// processes then retains those regions for as long as the app keeps emitting.
// Measured with v3/tests/event-performance:
//
//   - macOS 26.4.1 / WebKit-Cocoa: the retention lands in the HOST process,
//     visible in vmmap as "owned unmapped" — one region per oversized eval.
//     At a constant 4 MB/s, 8 KB payloads held the host flat at ~36 MB while
//     16 KB payloads climbed to 363 MB. 100 ev/s of 1 MB reached 11.5 GB.
//   - Ubuntu 26.04 / WebKitGTK 2.52.3: the mirror image — the host stays flat
//     and the WEB process grows instead, reaching 6.2 GB on the same scenario.
//     Its switchover sits higher, between 64 KB and 128 KB.
//   - Windows 11 / WebView2 151: not affected. The host holds flat at 65-77 MB
//     and the content process sawtooths under V8 GC in patched and unpatched
//     builds alike, so there is no transport retention to avoid. Routing large
//     payloads off the eval path still helps there — it delivers more bytes per
//     second while putting roughly half the pressure on the content process,
//     since an eval materialises both the source string and the parsed object.
//
// Instead, the payload is parked here under an unguessable id and the webview
// is told to fetch it from the asset server, which delivers via a URL scheme
// task and never touches the eval IPC path.
//
// Parked payloads are never expired by age. The original TTL sweep silently
// dropped any event whose fetch had not happened within 30s — a real scenario
// whenever the frontend's dispatch rate falls behind the producer's emit rate
// (large binary streams, busy UI) — and the webview's fetch failure was
// swallowed without a trace (wails#6619). Lossless eviction is provided by
// other means: the byte cap below degrades overflow to inline delivery, and
// dropWindow reclaims everything when the target window goes away. A page
// reload can strand payloads until that window closes; they count against the
// byte cap, so the worst case is a bounded shift toward inline delivery, not
// data loss.
const (
	// maxInlineEventPayload is the largest marshalled event JSON that may be
	// spliced directly into an eval. 8192 is the largest size measured at 0%
	// retention on macOS, which has the lowest knee of the three engines
	// (8-16 KB vs WebKitGTK's 64-128 KB; WebView2 has none). A single value is
	// therefore correct everywhere, just conservative off macOS: on Linux it
	// routes 8-128 KB payloads through HTTP that could have stayed inline,
	// costing a round trip rather than correctness.
	maxInlineEventPayload = 8192

	// eventPayloadStoreMaxBytes caps total parked bytes. On overflow the caller
	// falls back to inline delivery: that event then pays the out-of-line
	// retention cost, which is strictly better than dropping it or letting the
	// store grow without bound.
	eventPayloadStoreMaxBytes = 64 * 1024 * 1024

	eventPayloadPath = "/wails/eventpayload/"

	// eventPayloadIDLen is the hex length of an id: 16 random bytes.
	eventPayloadIDLen = 32
)

// isHexString reports whether s is entirely lowercase hex, the shape produced
// by hex.EncodeToString in put.
func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type parkedEventPayload struct {
	data     []byte
	windowID uint
}

// eventPayloadStore holds oversized event payloads awaiting a one-shot fetch
// from the webview.
type eventPayloadStore struct {
	mu     sync.Mutex
	items  map[string]parkedEventPayload
	bytes  int
	closed bool // guarded by mu; put refuses once shutdown has begun
}

func newEventPayloadStore() *eventPayloadStore {
	return &eventPayloadStore{
		items: make(map[string]parkedEventPayload),
	}
}

// put parks a payload and returns its id. ok is false when the store is full
// or shut down, in which case the caller must fall back to inline delivery.
func (s *eventPayloadStore) put(windowID uint, data []byte) (id string, ok bool) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", false
	}
	id = hex.EncodeToString(buf[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	// Refuse once shutdown has begun. Storing here afterwards would strand the
	// payload: nothing would ever reclaim it.
	if s.closed {
		return "", false
	}
	if s.bytes+len(data) > eventPayloadStoreMaxBytes {
		return "", false
	}
	s.items[id] = parkedEventPayload{data: data, windowID: windowID}
	s.bytes += len(data)
	return id, true
}

// take returns the payload for id exactly once. windowID must match the window
// the payload was dispatched to; a zero windowID skips the check, for platforms
// that do not tag asset requests with a window id.
func (s *eventPayloadStore) take(id string, windowID uint) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	item, found := s.items[id]
	if !found {
		return nil, false
	}
	if windowID != 0 && item.windowID != windowID {
		return nil, false
	}
	delete(s.items, id)
	s.bytes -= len(item.data)
	return item.data, true
}

// dropWindow discards anything parked for a window that is going away.
func (s *eventPayloadStore) dropWindow(windowID uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.items {
		if item.windowID == windowID {
			delete(s.items, id)
			s.bytes -= len(item.data)
		}
	}
}

// close refuses further payloads and reclaims everything parked. Safe to call
// more than once and concurrently with put.
func (s *eventPayloadStore) close() {
	s.mu.Lock()
	s.closed = true
	s.items = map[string]parkedEventPayload{}
	s.bytes = 0
	s.mu.Unlock()
}
