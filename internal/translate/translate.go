// Package translate converts between the Anthropic Messages wire and OpenAI chat-completions.
package translate

import (
	"errors"
	"io"
)

// STUB: workstream A replaces this file; signatures are the contract the router codes against.

var errTODO = errors.New("translate: not implemented")

// ToChatRequest converts an Anthropic Messages request body into a chat-completions body targeting wireModel.
func ToChatRequest(body []byte, wireModel string) (out []byte, stream bool, err error) {
	return nil, false, errTODO
}

// FromChatResponse converts a non-streaming chat-completions response into an Anthropic Messages response.
func FromChatResponse(body []byte, clientModel string) ([]byte, error) { return nil, errTODO }

// StreamChat converts a chat-completions SSE stream into Anthropic Messages SSE events, calling flush per event.
func StreamChat(w io.Writer, flush func(), r io.Reader, clientModel string) error { return errTODO }

// FromChatError maps an upstream error status/body into an Anthropic error body.
func FromChatError(status int, body []byte) []byte { return nil }

// CountTokens answers a Messages count_tokens request with a local estimate.
func CountTokens(body []byte) ([]byte, error) { return nil, errTODO }
