package telegram

import "testing"

func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello", 4096)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != "hello" {
		t.Errorf("expected hello, got %s", chunks[0])
	}
}

func TestSplitMessage_Long(t *testing.T) {
	// Create a message with paragraph breaks
	msg := ""
	for i := 0; i < 100; i++ {
		msg += "This is paragraph number " + string(rune('A'+i%26)) + ".\n\n"
	}

	chunks := splitMessage(msg, 200)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(chunks))
	}

	// Verify no chunk exceeds limit
	for i, chunk := range chunks {
		if len(chunk) > 200 {
			t.Errorf("chunk %d exceeds limit: %d chars", i, len(chunk))
		}
	}

	// Verify all content is preserved
	reassembled := ""
	for _, chunk := range chunks {
		reassembled += chunk
	}
	if reassembled != msg {
		t.Error("reassembled message doesn't match original")
	}
}
