package route

import "encoding/json"

// PromptInput is the shared input for all questions: one JSON string, as the
// Decisions API docs recommend for structured data. Questions refer to its
// fields by key path (`task`, `repo.size`). Only fields the questions use are
// sent; the raw file count is replaced by a named bucket.
func PromptInput(in Input) string {
	langs := in.Repo.Languages
	if langs == nil {
		langs = []string{}
	}
	b, _ := json.Marshal(map[string]any{
		"task": in.Task,
		"repo": map[string]any{"languages": langs, "size": in.Repo.SizeBucket(), "has_tests": in.Repo.HasTests},
	})
	return string(b)
}

// Questions is the fixed question set, sent in one request. The API scores each
// question independently against the shared input: questions cannot see each
// other and the model never sees the keys, so every instruction is a complete
// question, and the width question is asked unconditionally and ignored when
// the answer is to route.
//
// The harness descriptions are this spike's ASSUMED priors about what each
// harness is good at. They are not measured. They are the first thing the
// decision log should be used to correct.
func Questions() map[string]Question {
	return map[string]Question{
		"task_kind": {
			Type:         "choice",
			Instructions: "`task` is a request to a coding agent working in the repository described by `repo`. What kind of work is `task` mainly asking for?",
			Criteria: map[string]string{
				"bugfix":        "Fix behaviour that is wrong, crashing or failing.",
				"feature":       "Add new behaviour or a new capability.",
				"refactor":      "Restructure, rename, migrate or clean up code without changing what it does.",
				"test":          "Write or improve automated tests.",
				"docs":          "Write or update documentation, comments or a README.",
				"investigation": "Find out why something happens, measure, profile or explain, where what to change is not yet known.",
			},
		},
		"harness": {
			Type:         "choice",
			Instructions: "Which coding agent is the best single choice to carry out `task` in the repository described by `repo`?",
			Criteria: map[string]string{
				"claude-code": "Changes that span many files or need a lot of existing code read and kept consistent: refactors, migrations, investigations, loosely specified work.",
				"codex":       "Well-specified, self-contained changes with a clear check: a localized bug fix, a small feature, adding tests, a documentation edit.",
				"antigravity": "Front-end, UI, visual or browser-verified work, and work on Google-stack projects.",
				"refraktal":   "Work that is really several separable tasks, best split up and delegated to several agents by an orchestrator.",
			},
		},
		"fan_out": {
			Type:         "noul",
			Instructions: "Should several coding agents attempt `task` independently in parallel, so the best result can be picked, rather than one agent attempting it once?",
			Criteria: map[string]string{
				"true_description":  "The task is ambiguous, open to materially different valid solutions, or risky enough that comparing attempts is worth the extra cost.",
				"false_description": "The task is clear and one correct attempt is the likely outcome.",
			},
		},
		"fan_out_width": {
			Type:         "score",
			Instructions: "If `task` were attempted by several coding agents in parallel, how many independent attempts would be worth their cost?",
			Criteria: []string{
				"Two attempts: one second opinion is enough.",
				"Three attempts: several plausible approaches are worth comparing.",
				"Four attempts: the task is highly ambiguous or high-stakes and wide exploration is warranted.",
			},
		},
		"needs_decomposition": {
			Type:         "noul",
			Instructions: "Does `task` need to be broken into several separate subtasks before a coding agent starts work?",
			Criteria: map[string]string{
				"true_description":  "The task bundles several distinct deliverables or spans independent areas that would each be a separate change.",
				"false_description": "The task is one coherent change, even if it touches many files.",
			},
		},
	}
}
