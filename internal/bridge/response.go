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

	// Videos collected during response processing (from [artifact type="video"]
	// markers, e.g. the generate-video skill).
	Videos []Video
}

// Photo represents an image to be sent to the chat.
type Photo struct {
	Data    []byte // image bytes
	Caption string // optional caption
}

// Video represents a video to be sent to the chat.
type Video struct {
	Data    []byte // video bytes (e.g. mp4)
	Caption string // optional caption
}
