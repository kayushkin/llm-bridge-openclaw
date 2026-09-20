package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// The defect these tests pin: cutting a Go string at a fixed byte offset splits
// whatever rune straddles that offset, and the result is not valid UTF-8.
// encoding/json substitutes U+FFFD rather than failing, so nothing along the
// path reports it.
//
// A single hand-picked input is not enough. Whether a cut splits a rune depends
// on where that rune happens to sit relative to the budget, so one string lands
// off the boundary and passes against the unfixed code — which is how this
// survived across the fleet. Every test here slides the cut across a range and
// states how much of that range actually exercised the case.
//
// This repo is the odd one of the five. Its helper appends an ellipsis and it
// has TWO callers with TWO different budgets, so there are two call-site tests
// rather than one, and the budget assertions are made on the prefix rather than
// on the whole result.

// musicalNote is four bytes (U+1D11E). A two-byte rune is a weaker probe: only
// one of its two interior offsets is wrong, so a test that picks the other one
// is green against the bug.
const musicalNote = "\U0001D11E"

// ellipsis is the marker the helper appends when it actually cut something. It
// sits OUTSIDE maxBytes, which is pre-existing behaviour this fix deliberately
// preserves — whether it ought to count against the budget is the sweep's one
// parked decision and is not settled here.
const ellipsis = "..."

// TestTruncateAtRuneBoundaryWithEllipsisSlidesTheCutAcrossEveryOffset walks
// maxBytes across the whole length of an all-four-byte string. Three offsets in
// every four straddle a rune; the fourth is boundary-aligned and is the
// known-negative control, which must come back untouched against fixed and
// unfixed code alike.
func TestTruncateAtRuneBoundaryWithEllipsisSlidesTheCutAcrossEveryOffset(t *testing.T) {
	s := strings.Repeat(musicalNote, 125) // 500 bytes, the larger caller's budget
	straddled, aligned := 0, 0

	for maxBytes := 1; maxBytes < len(s); maxBytes++ {
		got := truncateAtRuneBoundaryWithEllipsis(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d: result is not valid UTF-8: %q", maxBytes, got)
		}
		// Every input here is longer than the budget, so the helper always cut
		// and the marker is always present. Strip it before measuring: the
		// budget governs the prefix, not the marker.
		if !strings.HasSuffix(got, ellipsis) {
			t.Fatalf("maxBytes=%d: result lost the ellipsis marker: %q", maxBytes, got)
		}
		prefix := strings.TrimSuffix(got, ellipsis)
		if len(prefix) > maxBytes {
			t.Fatalf("maxBytes=%d: prefix is %d bytes, over budget", maxBytes, len(prefix))
		}
		// Validity alone is not falsifiable — a helper that returns "" for
		// everything is always valid and always within budget. Pin maximality
		// too: whatever was dropped must genuinely not have fitted.
		_, width := utf8.DecodeRuneInString(s[len(prefix):])
		if len(prefix)+width <= maxBytes {
			t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
				maxBytes, len(prefix), width)
		}

		if maxBytes%4 == 0 {
			aligned++
			if prefix != s[:maxBytes] {
				t.Fatalf("maxBytes=%d is rune-aligned, so the cut is a no-op; prefix changed to %q", maxBytes, prefix)
			}
		} else {
			straddled++
		}
	}

	// The slide is a claim about coverage, so assert it. An earlier version of
	// this loop in the reference repo moved the input rather than the cut,
	// never straddled once, and was green against the unfixed helper.
	if straddled == 0 {
		t.Fatal("the cut never landed inside a rune: this test proves nothing")
	}
	if aligned == 0 {
		t.Fatal("the known-negative control never ran: cannot tell a real catch from a false one")
	}
	t.Logf("cut straddled a rune at %d of %d offsets (%d aligned controls)",
		straddled, straddled+aligned, aligned)
}

// TestTruncateAtRuneBoundaryWithEllipsisMixedWidths repeats the slide over a
// string mixing one-, two-, three- and four-byte runes, so the result does not
// depend on every rune being the same width.
func TestTruncateAtRuneBoundaryWithEllipsisMixedWidths(t *testing.T) {
	s := strings.Repeat("aé€"+musicalNote, 25) // 1+2+3+4 bytes per group
	straddled := 0

	for maxBytes := 1; maxBytes < len(s); maxBytes++ {
		got := truncateAtRuneBoundaryWithEllipsis(s, maxBytes)

		if !utf8.ValidString(got) {
			t.Fatalf("maxBytes=%d: result is not valid UTF-8: %q", maxBytes, got)
		}
		prefix := strings.TrimSuffix(got, ellipsis)
		if len(prefix) > maxBytes {
			t.Fatalf("maxBytes=%d: prefix is %d bytes, over budget", maxBytes, len(prefix))
		}
		_, width := utf8.DecodeRuneInString(s[len(prefix):])
		if len(prefix)+width <= maxBytes {
			t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
				maxBytes, len(prefix), width)
		}
		if len(prefix) < maxBytes {
			straddled++
		}
	}

	if straddled == 0 {
		t.Fatal("the cut never landed inside a rune: this test proves nothing")
	}
	t.Logf("cut straddled a rune at %d of %d offsets", straddled, len(s)-1)
}

func TestTruncateAtRuneBoundaryWithEllipsisEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		// ASCII is the other known-negative control: the whole class is a no-op
		// on it, so these must hold against the unfixed helper too.
		{"ascii under budget keeps no marker", "hello", 200, "hello"},
		{"ascii exactly at budget keeps no marker", "hello", 5, "hello"},
		{"ascii over budget cuts plainly and marks", "hello", 3, "hel..."},
		{"empty input", "", 200, ""},
		{"zero budget", "hello", 0, "..."},
		// A negative budget panicked before this fix (s[:-1]). Neither caller
		// can reach it — both pass compile-time constants — but the helper is
		// package-level and the next caller need not.
		{"negative budget", "hello", -1, "..."},
		// One four-byte rune against a budget too small to hold it: the only
		// rune-safe answer is the marker alone, not a lone lead byte.
		{"budget smaller than the first rune", musicalNote, 3, "..."},
		{"budget exactly the first rune keeps no marker", musicalNote, 4, musicalNote},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateAtRuneBoundaryWithEllipsis(c.in, c.maxBytes); got != c.want {
				t.Fatalf("truncateAtRuneBoundaryWithEllipsis(%q, %d) = %q, want %q",
					c.in, c.maxBytes, got, c.want)
			}
		})
	}
}

// TestTruncateAtRuneBoundaryWithEllipsisLeavesTheBudgetArithmeticAlone is this
// repo's own known-negative: the fix moves where the cut lands and must not
// move how the marker is budgeted. If a later change decides the ellipsis
// belongs inside maxBytes — the sweep's parked decision — this test is the one
// that should be edited deliberately, rather than the change passing silently.
func TestTruncateAtRuneBoundaryWithEllipsisLeavesTheBudgetArithmeticAlone(t *testing.T) {
	got := truncateAtRuneBoundaryWithEllipsis(strings.Repeat("a", 300), 200)
	if want := strings.Repeat("a", 200) + ellipsis; got != want {
		t.Fatalf("ellipsis budgeting changed: got %d bytes, want %d", len(got), len(want))
	}
	if len(got) != 203 {
		t.Fatalf("result is %d bytes; the marker has always sat outside the 200-byte budget", len(got))
	}
}

// TestToolResultOutputStaysValidUTF8 is the tier-1 call site. The helper being
// correct does not prove the caller uses it, and this caller is what makes the
// defect reach a user: translateToolResult puts the cut string in
// msg.ToolResultEvent.Output, translateEntry hands the event to the Tailer, and
// emitEvent json.Marshals it as one NDJSON line to stdout for bridge-server.
// json.Marshal swaps a split rune for U+FFFD and reports no error, so the
// assertion is made on the encoded form — what bridge-server actually receives.
//
// The text slides so a four-byte rune lands on each of the four byte offsets
// around the 500-byte budget.
func TestToolResultOutputStaysValidUTF8(t *testing.T) {
	for pad := 497; pad <= 500; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			text := strings.Repeat("a", pad) + musicalNote + strings.Repeat("b", 50)
			content, err := json.Marshal([]contentBlock{{Type: "text", Text: text}})
			if err != nil {
				t.Fatalf("marshal content: %v", err)
			}

			events := translateToolResult(
				jsonlMessage{Role: "toolResult", ToolName: "Bash", Content: content},
				"bsid-utf8", "hsid-utf8", []byte("{}"))
			if len(events) != 1 {
				t.Fatalf("want 1 tool-result event, got %d", len(events))
			}
			if events[0].ToolResult == nil {
				t.Fatal("event carries no ToolResult")
			}

			output := events[0].ToolResult.Output
			if !utf8.ValidString(output) {
				t.Fatalf("tool-result output is not valid UTF-8: %q", output)
			}
			if len(strings.TrimSuffix(output, ellipsis)) > 500 {
				t.Fatalf("output prefix is %d bytes, over the 500-byte budget",
					len(strings.TrimSuffix(output, ellipsis)))
			}
			encoded, err := json.Marshal(events[0])
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if strings.Contains(string(encoded), "�") {
				t.Fatalf("tool-result output reached JSON carrying U+FFFD: %s", encoded)
			}
		})
	}
}

// TestHTTPErrorBodyStaysValidUTF8 is the second call site, and it is the reason
// this repo needs two. It has its own budget (200, not 500) and its own
// consequence: the cut string becomes an error message rather than a canonical
// event field, so reverting this call site alone is a different sabotage row
// from reverting the other one.
func TestHTTPErrorBodyStaysValidUTF8(t *testing.T) {
	for pad := 197; pad <= 200; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			body := strings.Repeat("a", pad) + musicalNote + strings.Repeat("b", 50)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, body)
			}))
			defer srv.Close()

			cfg := &Config{OpenClawURL: srv.URL}
			err := sendToOpenClaw(context.Background(), cfg, "agent", "session", "hi")
			if err == nil {
				t.Fatal("want an error from a 500 response, got nil")
			}

			got := err.Error()
			if !utf8.ValidString(got) {
				t.Fatalf("error message is not valid UTF-8: %q", got)
			}
			// The message is prose the user reads, and it is also marshalled
			// into events elsewhere; assert on the encoded form for the same
			// reason as the tool-result test.
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal error message: %v", err)
			}
			if strings.Contains(string(encoded), "�") {
				t.Fatalf("error message reached JSON carrying U+FFFD: %s", encoded)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The two tests below pin the call-site budgets themselves, which the six above
// do not. Every test above this line moves a direction or a mechanism: the
// helper is asked to cut correctly at whatever budget it is handed, and the
// call-site tests slide their padding around the budget the caller passes. None
// of them observes the NUMBER. Measured with scripts/sabotage-truncation.py:
// with only the tests above, 500 -> 499, 200 -> 199 and 200 -> 201 all pass the
// whole suite. Only 500 -> 501 goes red, and only because one assertion happens
// to name that side.
//
// These are not defect fixes. Both budgets are correct values that nothing was
// holding in place; this is insurance, and reading it as three live bugs would
// be wrong.
//
// Two things about how they are written, both of which are the point:
//
//   * They MARK A CHARACTER rather than assert a length. A cut that keeps the
//     wrong end of the string (s[len(s)-n:] where s[:n] was meant) returns
//     exactly the right length and passes any length assertion. The marker at
//     the last in-budget offset must survive and the marker at the first
//     out-of-budget offset must not, so the test names which side the cut fell
//     on rather than how far along it was.
//
//   * They spell the budget out. A test that derives its expectation from the
//     same constant the code compares against moves with it and is green
//     against the drift it exists to catch. There is no named constant here to
//     import, but the rule is the reason the number is written twice.

// boundaryMarked builds an ASCII string longer than budget with a unique
// character at the last in-budget offset and another at the first offset past
// it. Both markers are single-byte, so nothing here depends on rune widths —
// this test asks where the cut landed, not whether it split a rune.
//
// It returns the string plus the two markers so a caller cannot pass one budget
// to the fixture and assert against another.
func boundaryMarked(budget int) (text, lastIn, firstOut string) {
	lastIn, firstOut = "X", "Y"
	body := []byte(strings.Repeat("a", budget+50))
	body[budget-1] = lastIn[0]
	body[budget] = firstOut[0]
	return string(body), lastIn, firstOut
}

// TestToolResultOutputIsCutAtExactlyFiveHundredBytes pins translate.go's budget.
func TestToolResultOutputIsCutAtExactlyFiveHundredBytes(t *testing.T) {
	text, lastIn, firstOut := boundaryMarked(500)
	// Guard the fixture before trusting it: "contains" is only an answer about
	// position while each marker occurs exactly once.
	if strings.Count(text, lastIn) != 1 || strings.Count(text, firstOut) != 1 {
		t.Fatalf("fixture is ambiguous: %d in-budget markers, %d out-of-budget markers",
			strings.Count(text, lastIn), strings.Count(text, firstOut))
	}

	content, err := json.Marshal([]contentBlock{{Type: "text", Text: text}})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	events := translateToolResult(
		jsonlMessage{Role: "toolResult", ToolName: "Bash", Content: content},
		"bsid-budget", "hsid-budget", []byte("{}"))
	if len(events) != 1 {
		t.Fatalf("want 1 tool-result event, got %d", len(events))
	}
	if events[0].ToolResult == nil {
		t.Fatal("event carries no ToolResult")
	}

	output := events[0].ToolResult.Output
	if !strings.Contains(output, lastIn) {
		t.Fatalf("the byte at offset 499 was dropped, so the tool-result budget is below 500: %q", output)
	}
	if strings.Contains(output, firstOut) {
		t.Fatalf("the byte at offset 500 was kept, so the tool-result budget is above 500: %q", output)
	}
}

// TestHTTPErrorBodyIsCutAtExactlyTwoHundredBytes pins client.go's budget. The
// test above it, TestHTTPErrorBodyStaysValidUTF8, asserts validity only and
// makes no claim about length in either direction.
func TestHTTPErrorBodyIsCutAtExactlyTwoHundredBytes(t *testing.T) {
	body, lastIn, firstOut := boundaryMarked(200)
	if strings.Count(body, lastIn) != 1 || strings.Count(body, firstOut) != 1 {
		t.Fatalf("fixture is ambiguous: %d in-budget markers, %d out-of-budget markers",
			strings.Count(body, lastIn), strings.Count(body, firstOut))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	cfg := &Config{OpenClawURL: srv.URL}
	err := sendToOpenClaw(context.Background(), cfg, "agent", "session", "hi")
	if err == nil {
		t.Fatal("want an error from a 500 response, got nil")
	}

	got := err.Error()
	if !strings.Contains(got, lastIn) {
		t.Fatalf("the byte at offset 199 was dropped, so the error-body budget is below 200: %q", got)
	}
	if strings.Contains(got, firstOut) {
		t.Fatalf("the byte at offset 200 was kept, so the error-body budget is above 200: %q", got)
	}
}
