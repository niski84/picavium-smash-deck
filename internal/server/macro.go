package server

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// adjacentKeys maps each lowercase letter to its QWERTY neighbours.
// Used by humanizeText to pick realistic typos.
var adjacentKeys = map[rune][]rune{
	'a': {'s', 'q', 'w', 'z'},
	'b': {'v', 'n', 'g', 'h'},
	'c': {'x', 'v', 'd', 'f'},
	'd': {'s', 'f', 'e', 'r', 'c', 'x'},
	'e': {'w', 'r', 'd', 's'},
	'f': {'d', 'g', 'r', 't', 'v', 'c'},
	'g': {'f', 'h', 't', 'y', 'b', 'v'},
	'h': {'g', 'j', 'y', 'u', 'n', 'b'},
	'i': {'u', 'o', 'k', 'j'},
	'j': {'h', 'k', 'u', 'i', 'm', 'n'},
	'k': {'j', 'l', 'i', 'o', 'm'},
	'l': {'k', 'o', 'p'},
	'm': {'n', 'j', 'k'},
	'n': {'b', 'm', 'h', 'j'},
	'o': {'i', 'p', 'l', 'k'},
	'p': {'o', 'l'},
	'q': {'w', 'a'},
	'r': {'e', 't', 'f', 'd'},
	's': {'a', 'd', 'w', 'e', 'x', 'z'},
	't': {'r', 'y', 'g', 'f'},
	'u': {'y', 'i', 'h', 'j'},
	'v': {'c', 'b', 'f', 'g'},
	'w': {'q', 'e', 'a', 's'},
	'x': {'z', 'c', 's', 'd'},
	'y': {'t', 'u', 'g', 'h'},
	'z': {'x', 'a', 's'},
}

// humanizeText inserts realistic QWERTY typos (wrong neighbour key + backspace)
// and occasional double-presses into text to simulate human typing.
// Backspace is \b (0x08), which kvmd maps to the Backspace key in the en-us keymap.
func humanizeText(text string, rng *rand.Rand) string {
	var buf strings.Builder
	buf.Grow(len(text) * 2)

	for _, ch := range text {
		lower := unicode.ToLower(ch)
		neighbours := adjacentKeys[lower]

		// ~7% chance: hit an adjacent key then correct it
		if len(neighbours) > 0 && rng.Float64() < 0.07 {
			wrong := neighbours[rng.Intn(len(neighbours))]
			if unicode.IsUpper(ch) {
				wrong = unicode.ToUpper(wrong)
			}
			buf.WriteRune(wrong)
			buf.WriteByte('\b') // Backspace
		}

		// ~2% chance: double-press then correct
		if rng.Float64() < 0.02 {
			buf.WriteRune(ch)
			buf.WriteByte('\b')
		}

		buf.WriteRune(ch)
	}

	return buf.String()
}

// typeMacro handles POST /api/type-macro.
// It resolves tokens server-side (already done by client, but kept for safety),
// optionally humanises the text, then sends it to the PiKVM with an appropriate delay.
func (h *handlers) typeMacro(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}

	text := r.FormValue("text")
	keymap := r.FormValue("keymap")
	humanMode := r.FormValue("human") == "1"

	if strings.TrimSpace(text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "text is empty",
		})
		return
	}

	if humanMode {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		text = humanizeText(text, rng)
	}

	// PiKVM delay param is in seconds. 0.08s = 80ms per keystroke ≈ realistic human pace.
	// Each keystroke = press + release events, so wall-clock time ≈ delay×2 per char.
	// Timeout: chars × 0.16s/char + 10s buffer.
	charCount := len([]rune(text))
	var timeout time.Duration
	if humanMode {
		timeout = time.Duration(float64(charCount)*0.17*float64(time.Second)) + 10*time.Second
	} else {
		timeout = 15 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var err error
	if humanMode {
		err = kvm.TypeTextWithDelay(ctx, text, keymap, 0.08)
	} else {
		err = kvm.TypeText(ctx, text, keymap)
	}
	if err != nil {
		writeErr(w, err)
		return
	}

	writeOK(w, fmt.Sprintf("macro sent (%d chars)", len([]rune(text))))
}
