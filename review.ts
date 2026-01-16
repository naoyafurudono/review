import { query } from "@anthropic-ai/claude-agent-sdk";
import { execSync } from "child_process";

// Get prompt from command line arguments
const args = process.argv.slice(2);
if (args.length === 0) {
  console.error("Usage: npm start -- <prompt>");
  process.exit(1);
}
const userPrompt = args.join(" ");

// Call reviewer Claude Code to answer a question
function askReviewer(questions: any[]): Record<string, string> {
  // Format questions for the reviewer
  let reviewerPrompt =
    "You are a reviewer for Claude Code's work.\n" +
    "Answer the following questions by selecting the best option.\n" +
    "Return ONLY the option number (1, 2, 3...) for each question.\n\n";

  for (let i = 0; i < questions.length; i++) {
    const q = questions[i];
    reviewerPrompt += `Question ${i + 1}: ${q.question}\n`;
    if (q.options && q.options.length > 0) {
      reviewerPrompt += "Options:\n";
      for (let j = 0; j < q.options.length; j++) {
        const opt = q.options[j];
        reviewerPrompt += `  ${j + 1}. ${opt.label}: ${opt.description}\n`;
      }
    }
    reviewerPrompt += "\n";
  }

  console.error("[review] Calling reviewer...");
  console.error("[review] Reviewer prompt:", reviewerPrompt);

  try {
    // Call reviewer Claude Code with read-only tools
    const output = execSync(
      `claude -p "${reviewerPrompt.replace(/"/g, '\\"')}" --allowedTools "Read,Glob,Grep"`,
      { encoding: "utf-8", timeout: 60000 }
    );

    console.error("[review] Reviewer response:", output.trim());

    // Parse the answer - look for digits
    const answers: Record<string, string> = {};
    const answerText = output.trim();

    for (let i = 0; i < questions.length; i++) {
      const q = questions[i];
      // Default to first option
      let selectedIndex = 0;

      // Try to find a digit in the response
      for (const char of answerText) {
        if (char >= "1" && char <= "9") {
          selectedIndex = parseInt(char) - 1;
          break;
        }
      }

      // Make sure index is valid
      if (selectedIndex >= q.options.length) {
        selectedIndex = 0;
      }

      // Map question text to selected option label
      answers[q.question] = q.options[selectedIndex]?.label || q.options[0]?.label;
    }

    console.error("[review] Parsed answers:", answers);
    return answers;
  } catch (error) {
    console.error("[review] Reviewer error:", error);
    // Default to first option for all questions
    const answers: Record<string, string> = {};
    for (const q of questions) {
      answers[q.question] = q.options[0]?.label || "option1";
    }
    return answers;
  }
}

// Main function
async function main() {
  console.error("[review] Starting worker with prompt:", userPrompt);

  for await (const message of query({
    prompt: userPrompt,
    options: {
      // canUseTool callback handles AskUserQuestion
      canUseTool: async (toolName, input) => {
        console.error(`[review] Tool request: ${toolName}`);

        if (toolName === "AskUserQuestion") {
          console.error("[review] Detected AskUserQuestion");
          console.error("[review] Questions:", JSON.stringify(input, null, 2));

          // Call reviewer to answer the questions
          const questions = (input as any).questions || [];
          const answers = askReviewer(questions);

          console.error("[review] Returning answers to worker");

          // Return the answers to continue the worker
          return {
            behavior: "allow" as const,
            updatedInput: {
              questions: questions,
              answers: answers,
            },
          };
        }

        // Auto-approve other tools
        return { behavior: "allow" as const, updatedInput: input };
      },
    },
  })) {
    // Output messages
    if ("result" in message) {
      console.log(message.result);
    } else if (message.type === "assistant") {
      // Stream assistant messages
      const content = (message as any).message?.content;
      if (content) {
        for (const item of content) {
          if (item.type === "text" && item.text) {
            process.stdout.write(item.text);
          }
        }
      }
    }
  }
}

main().catch((error) => {
  console.error("Error:", error);
  process.exit(1);
});
