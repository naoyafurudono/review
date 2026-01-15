package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// StreamMessage represents a message from Claude Code's stream-json output
type StreamMessage struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
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
	Type   string `json:"type"`
	Answer Answer `json:"answer"`
}

// Answer represents the answer structure
type Answer struct {
	ToolUseID string            `json:"tool_use_id"`
	Answers   map[string]string `json:"answers"`
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
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	var wg sync.WaitGroup

	// Process stderr
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(os.Stderr, stderr)
	}()

	// Process stdout and handle questions
	wg.Add(1)
	go func() {
		defer wg.Done()
		processWorkerOutput(stdout, stdin)
	}()

	wg.Wait()
	return cmd.Wait()
}

func processWorkerOutput(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	// Increase buffer size for large JSON messages
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

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
			var assistant AssistantMessage
			if err := json.Unmarshal([]byte(line), &assistant); err != nil {
				continue
			}

			for _, content := range assistant.Content {
				if content.Type == "tool_use" && content.Name == "AskUserQuestion" {
					var input AskUserQuestionInput
					if err := json.Unmarshal(content.Input, &input); err != nil {
						continue
					}

					answer := askReviewer(&input)
					response := createResponse(content.ID, answer)

					responseJSON, err := json.Marshal(response)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Failed to marshal response: %v\n", err)
						continue
					}

					fmt.Fprintln(w, string(responseJSON))
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Scanner error: %v\n", err)
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
	return UserResponse{
		Type: "user_input_result",
		Answer: Answer{
			ToolUseID: toolUseID,
			Answers:   answers,
		},
	}
}
