package bridge

// AgentResponse is the typed response from bridge to callers.
// It contains the cleaned text (all directives stripped) plus any
// side-effect outputs collected during response processing.
type AgentResponse struct {
	// Text is the cleaned response with all directives stripped.
	Text string

	// Photos collected during response processing (from [generate-image]
	// directives and [artifact type="image"] markers).
	Photos []Photo
}

// Photo represents an image to be sent to the chat.
type Photo struct {
	Data    []byte // image bytes
	Caption string // optional caption
}
