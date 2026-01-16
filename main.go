package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/creack/pty"
)

// StreamMessage represents a message from Claude Code's stream-json output
type StreamMessage struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
}

// AssistantStreamMessage represents the assistant stream message wrapper
type AssistantStreamMessage struct {
	Type    string           `json:"type"`
	Message AssistantMessage `json:"message"`
}

// AssistantMessage represents the assistant's message content
type AssistantMessage struct {
	Role    string        `json:"role"`
	Content []ContentItem `json:"content"`
}

// ContentItem represents a content item in the message
type ContentItem struct {
	Type      string          `json:"type"`
	ToolUse   *ToolUse        `json:"tool_use,omitempty"`
	Text      string          `json:"text,omitempty"`
	Questions []Question      `json:"questions,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// ToolUse represents a tool use request
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// AskUserQuestionInput represents the input for AskUserQuestion tool
type AskUserQuestionInput struct {
	Questions []Question `json:"questions"`
}

// Question represents a question asked to the user
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

// Option represents an option for a question
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// UserResponse represents a response to send back to Claude Code
type UserResponse struct {
	Type    string      `json:"type"`
	Message UserMessage `json:"message"`
}

// UserMessage represents the user message content
type UserMessage struct {
	Role    string       `json:"role"`
	Content []ToolResult `json:"content"`
}

// ToolResult represents a tool result to send back
type ToolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: review <prompt>")
		os.Exit(1)
	}

	prompt := strings.Join(os.Args[1:], " ")

	if err := run(prompt); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(prompt string) error {
	// Start worker Claude Code with stream-json format
	cmd := exec.Command("claude",
		"-p", prompt,
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	)

	// Use PTY to start the command (this handles stdout buffering)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start command with pty: %w", err)
	}
	defer ptmx.Close()

	// PTY provides both read (stdout) and write (stdin) on same fd
	// Process output and send responses through the same PTY
	processWorkerOutput(ptmx, ptmx)

	return cmd.Wait()
}

func processWorkerOutput(r io.Reader, w io.Writer) {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Read error: %v\n", err)
			}
			break
		}

		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")

		if len(line) == 0 {
			continue
		}

		// Skip echo of our own input (PTY echoes back what we write)
		if strings.HasPrefix(line, `{"type":"user_input_result"`) {
			continue
		}

		// Try to parse as stream message
		var msg StreamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			fmt.Println(line)
			continue
		}

		// Output non-question messages
		fmt.Println(line)

		// Check for tool use
		if msg.Type == "assistant" {
			var streamMsg AssistantStreamMessage
			if err := json.Unmarshal([]byte(line), &streamMsg); err != nil {
				continue
			}

			for _, content := range streamMsg.Message.Content {
				if content.Type == "tool_use" && content.Name == "AskUserQuestion" {
					var input AskUserQuestionInput
					if err := json.Unmarshal(content.Input, &input); err != nil {
						continue
					}

					// Call reviewer to answer the question
					answer := askReviewer(&input)

					// Send response back to worker
					response := createResponse(content.ID, answer)
					responseJSON, err := json.Marshal(response)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Failed to marshal response: %v\n", err)
						continue
					}

					// Write response followed by newline
					w.Write([]byte(string(responseJSON) + "\n"))
				}
			}
		}
	}
}

func askReviewer(input *AskUserQuestionInput) map[string]string {
	// Format question for reviewer
	var sb strings.Builder
	sb.WriteString("あなたはClaude Codeの作業をレビューするレビュワーです。\n")
	sb.WriteString("以下の質問に対して、最適な選択肢を選んで回答してください。\n")
	sb.WriteString("回答は選択肢の番号（1, 2, 3...）のみを返してください。\n\n")

	for i, q := range input.Questions {
		sb.WriteString(fmt.Sprintf("質問%d: %s\n", i+1, q.Question))
		if len(q.Options) > 0 {
			sb.WriteString("選択肢:\n")
			for j, opt := range q.Options {
				sb.WriteString(fmt.Sprintf("  %d. %s: %s\n", j+1, opt.Label, opt.Description))
			}
		}
		sb.WriteString("\n")
	}

	// Call reviewer Claude Code
	cmd := exec.Command("claude",
		"-p", sb.String(),
		"--allowedTools", "Read,Glob,Grep",
	)

	output, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Reviewer error: %v\n", err)
		// Default to first option
		answers := make(map[string]string)
		for i := range input.Questions {
			answers[fmt.Sprintf("q%d", i)] = "0"
		}
		return answers
	}

	// Parse reviewer's answer
	answerText := strings.TrimSpace(string(output))
	answers := make(map[string]string)

	// Try to extract number from answer
	for i := range input.Questions {
		// Default to first option (index 0)
		answers[fmt.Sprintf("q%d", i)] = "0"
	}

	// Simple parsing: look for digits
	for _, char := range answerText {
		if char >= '1' && char <= '9' {
			// Convert to 0-based index
			answers["q0"] = fmt.Sprintf("%d", char-'1')
			break
		}
	}

	return answers
}

func createResponse(toolUseID string, answers map[string]string) UserResponse {
	// Format the answer as a simple string (e.g., "q0: 1")
	var parts []string
	for k, v := range answers {
		parts = append(parts, fmt.Sprintf("%s: %s", k, v))
	}
	content := strings.Join(parts, ", ")

	return UserResponse{
		Type: "user",
		Message: UserMessage{
			Role: "user",
			Content: []ToolResult{
				{
					Type:      "tool_result",
					ToolUseID: toolUseID,
					Content:   content,
				},
			},
		},
	}
}
