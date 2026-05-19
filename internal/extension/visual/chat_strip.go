package visual

import (
	"moonbridge/internal/protocol/chat"
)

// StripImagesFromChat strips image_url content parts from a chat.ChatRequest
// and replaces them with text placeholders that reference the image by index.
//
// Returns the stripped request and whether any images were found. Use this on
// the streaming chat path where the visual orchestrator does not run but the
// model would otherwise receive raw base64 image data that it cannot consume
// (text-only upstreams) and that wastes tokens.
//
// Mirrors StripImagesFromAnthropic for the openai-chat protocol.
func StripImagesFromChat(req chat.ChatRequest) (chat.ChatRequest, bool) {
	modified := false
	imageIndex := 0

	out := req
	out.Messages = make([]chat.ChatMessage, len(req.Messages))
	copy(out.Messages, req.Messages)

	for mi := range out.Messages {
		msg := &out.Messages[mi]
		parts, ok := msg.Content.([]chat.ContentPart)
		if !ok {
			continue
		}
		newParts := make([]chat.ContentPart, 0, len(parts))
		for _, part := range parts {
			if part.Type == "image_url" {
				imageIndex++
				newParts = append(newParts, chat.ContentPart{
					Type: "text",
					Text: visualAttachmentText(imageIndex),
				})
				modified = true
				continue
			}
			newParts = append(newParts, part)
		}
		msg.Content = newParts
	}
	return out, modified
}
