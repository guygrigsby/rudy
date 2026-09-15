package acptest

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const siblingPrefix = "rudy-4tf.1."

type dependencyEdge struct {
	From string
	To   string
}

func (e dependencyEdge) String() string { return e.From + " -> " + e.To }

func TestACPPlanDependenciesAgreeEverywhere(t *testing.T) {
	root := repositoryRoot(t)
	planPath := filepath.Join(root, "docs/specs/2026-09-14-acp-remote-runtime-plan.md")
	plan, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}

	declared, tasks := declaredDependencies(t, string(plan))
	diagram := mermaidDependencies(t, string(plan), tasks)
	beads := beadDependencies(t, filepath.Join(root, ".beads/issues.jsonl"), tasks)
	compareEdges(t, "task declarations", declared, "Mermaid diagram", diagram)
	compareEdges(t, "task declarations", declared, "Beads blocks", beads)
}

func TestMermaidDependenciesExpandsChainedEdges(t *testing.T) {
	plan := "```mermaid\n" +
		"flowchart LR\n" +
		"    B[4tf.1.2 wire] --> A[4tf.1.1 catalogue] --> J[4tf.1.11 durability]\n" +
		"```\n"
	tasks := []string{"rudy-4tf.1.1", "rudy-4tf.1.2", "rudy-4tf.1.11"}

	got := mermaidDependencies(t, plan, tasks)
	want := []dependencyEdge{
		{From: "rudy-4tf.1.1", To: "rudy-4tf.1.11"},
		{From: "rudy-4tf.1.2", To: "rudy-4tf.1.1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chained Mermaid edges = %v, want %v", got, want)
	}
}

func TestMermaidDependenciesIgnoresEdgesOutsideDependencyDiagram(t *testing.T) {
	plan := "A --> J\n\n" +
		"```mermaid\n" +
		"flowchart LR\n" +
		"    A[4tf.1.1 catalogue] --> B[4tf.1.2 wire]\n" +
		"    J[4tf.1.11 durability]\n" +
		"```\n"
	tasks := []string{"rudy-4tf.1.1", "rudy-4tf.1.2", "rudy-4tf.1.11"}

	got := mermaidDependencies(t, plan, tasks)
	want := []dependencyEdge{{From: "rudy-4tf.1.1", To: "rudy-4tf.1.2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("dependency diagram edges = %v, want %v", got, want)
	}
}

func TestBeadDependenciesRejectsUndeclaredSiblingEndpoints(t *testing.T) {
	if probe := os.Getenv("RUDY_ACP_BEAD_DEPENDENCY_PROBE"); probe != "" {
		beadDependencies(t, probe, []string{"rudy-4tf.1.1"})
		return
	}

	cases := []struct {
		name    string
		records string
	}{
		{
			name:    "declared task blocked by undeclared sibling",
			records: "{\"id\":\"rudy-4tf.1.1\",\"dependencies\":[{\"depends_on_id\":\"rudy-4tf.1.99\",\"type\":\"blocks\"}]}\n",
		},
		{
			name: "extra sibling issue carries blocking edge",
			records: "{\"id\":\"rudy-4tf.1.1\",\"dependencies\":[]}\n" +
				"{\"id\":\"rudy-4tf.1.99\",\"dependencies\":[{\"depends_on_id\":\"rudy-4tf.1.1\",\"type\":\"blocks\"}]}\n",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "issues.jsonl")
			if err := os.WriteFile(path, []byte(test.records), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestBeadDependenciesRejectsUndeclaredSiblingEndpoints$")
			cmd.Env = append(os.Environ(), "RUDY_ACP_BEAD_DEPENDENCY_PROBE="+path)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("accepted undeclared sibling endpoint:\n%s", output)
			}
			if !strings.Contains(string(output), "undeclared sibling") {
				t.Fatalf("wrong undeclared sibling failure:\n%s", output)
			}
		})
	}
}

func TestVendorTypesRejectsACPImportOutsideAdapters(t *testing.T) {
	root := repositoryRoot(t)
	assertVendorTypesRejects(t, root, filepath.Join(root, "internal"))
}

func TestVendorTypesRejectsACPImportOutsideInternal(t *testing.T) {
	root := repositoryRoot(t)
	assertVendorTypesRejects(t, root, filepath.Join(root, "cmd"))
}

func assertVendorTypesRejects(t *testing.T, root, parent string) {
	t.Helper()
	probe, err := os.MkdirTemp(parent, "acpguardprobe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probe) })
	source := "package acpguardprobe\n\nimport _ \"github.com/coder/" + "acp-go-sdk\"\n"
	if err := os.WriteFile(filepath.Join(probe, "probe.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runVendorTypes(root)
	if err == nil {
		t.Fatalf("vendor-types accepted ACP SDK import in %s", probe)
	}
	if !strings.Contains(string(output), "acp sdk outside its adapters:") || !strings.Contains(string(output), filepath.Base(probe)) {
		t.Fatalf("vendor-types returned the wrong failure:\n%s", output)
	}
}

func TestVendorTypesAllowsACPImportsInBothAdapters(t *testing.T) {
	root := repositoryRoot(t)
	for _, adapter := range []string{"acpagent", "acpclient"} {
		dir := filepath.Join(root, "internal", adapter)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		probe := filepath.Join(dir, "vendor_guard_probe.go")
		t.Cleanup(func() { _ = os.Remove(probe) })
		source := "package " + adapter + "\n\nimport _ \"github.com/coder/" + "acp-go-sdk\"\n"
		if err := os.WriteFile(probe, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if output, err := runVendorTypes(root); err != nil {
		t.Fatalf("vendor-types rejected an allowed adapter import: %v\n%s", err, output)
	}
}

func runVendorTypes(root string) ([]byte, error) {
	cmd := exec.Command("make", "vendor-types")
	cmd.Dir = root
	return cmd.CombinedOutput()
}

func declaredDependencies(t *testing.T, plan string) ([]dependencyEdge, []string) {
	t.Helper()
	starts := regexp.MustCompile(`(?m)^## Task [^\n]+`).FindAllStringIndex(plan, -1)
	if len(starts) == 0 {
		t.Fatal("implementation plan has no tasks")
	}
	beadPattern := regexp.MustCompile("(?m)^Bead: `(" + regexp.QuoteMeta(siblingPrefix) + "[0-9]+)`$")
	dependencyPattern := regexp.MustCompile("(?m)^Depends on ([^\n]+)\\.$")
	siblingPattern := regexp.MustCompile("`(" + regexp.QuoteMeta(siblingPrefix) + "[0-9]+)`")

	var edges []dependencyEdge
	var tasks []string
	seenTasks := make(map[string]bool)
	for i := range starts {
		start := starts[i]
		end := len(plan)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		block := plan[start[0]:end]
		beadMatch := beadPattern.FindStringSubmatch(block)
		if len(beadMatch) != 2 {
			t.Fatalf("%q lacks one explicit Bead declaration", startText(plan, start))
		}
		bead := beadMatch[1]
		if seenTasks[bead] {
			t.Fatalf("task Bead %s is declared more than once", bead)
		}
		seenTasks[bead] = true
		tasks = append(tasks, bead)
		dependencyMatches := dependencyPattern.FindAllStringSubmatch(block, -1)
		if len(dependencyMatches) != 1 {
			t.Fatalf("task %s lacks one explicit Depends on declaration", bead)
		}
		dependencyMatch := dependencyMatches[0]
		if dependencyMatch[1] == "no sibling bead" {
			continue
		}
		dependencies := siblingPattern.FindAllStringSubmatch(dependencyMatch[1], -1)
		if len(dependencies) == 0 {
			t.Fatalf("task %s has malformed dependency declaration %q", bead, dependencyMatch[1])
		}
		for _, match := range dependencies {
			edges = append(edges, dependencyEdge{From: match[1], To: bead})
		}
	}
	slices.Sort(tasks)
	return canonicalEdges(t, edges), tasks
}

func mermaidDependencies(t *testing.T, plan string, tasks []string) []dependencyEdge {
	t.Helper()
	nodePattern := regexp.MustCompile(`\b([A-Z]+)\[([^]]*?4tf\.1\.[0-9]+)[^]]*\]`)
	shortTask := regexp.MustCompile(`4tf\.1\.[0-9]+`)
	diagramPattern := regexp.MustCompile("(?s)```mermaid[^\\n]*\\n(.*?)\\n```")
	var diagram string
	for _, match := range diagramPattern.FindAllStringSubmatch(plan, -1) {
		if !shortTask.MatchString(match[1]) {
			continue
		}
		if diagram != "" {
			t.Fatal("implementation plan has more than one sibling dependency diagram")
		}
		diagram = match[1]
	}
	if diagram == "" {
		t.Fatal("implementation plan has no sibling dependency diagram")
	}
	edgeNodePattern := regexp.MustCompile(`^\s*([A-Z]+)(?:\[[^]]+\])?\s*$`)
	nodes := make(map[string]string)
	for _, match := range nodePattern.FindAllStringSubmatch(diagram, -1) {
		nodes[match[1]] = "rudy-" + shortTask.FindString(match[2])
	}
	for _, task := range tasks {
		found := false
		for _, nodeTask := range nodes {
			if nodeTask == task {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Mermaid diagram has no node for task %s", task)
		}
	}
	var edges []dependencyEdge
	for _, line := range strings.Split(diagram, "\n") {
		if !strings.Contains(line, "-->") {
			continue
		}
		parts := strings.Split(line, "-->")
		tasksInChain := make([]string, 0, len(parts))
		for _, part := range parts {
			match := edgeNodePattern.FindStringSubmatch(part)
			if len(match) != 2 {
				t.Fatalf("malformed sibling Mermaid edge %q", line)
			}
			task, ok := nodes[match[1]]
			if !ok {
				t.Fatalf("Mermaid edge has undeclared task node %q", line)
			}
			tasksInChain = append(tasksInChain, task)
		}
		for i := range len(tasksInChain) - 1 {
			edges = append(edges, dependencyEdge{From: tasksInChain[i], To: tasksInChain[i+1]})
		}
	}
	return canonicalEdges(t, edges)
}

func beadDependencies(t *testing.T, path string, tasks []string) []dependencyEdge {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	taskSet := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		taskSet[task] = true
	}
	siblingPattern := regexp.MustCompile(`^` + regexp.QuoteMeta(siblingPrefix) + `[0-9]+$`)
	var edges []dependencyEdge
	seenTasks := make(map[string]bool, len(tasks))
	scanner := bufio.NewScanner(f)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	for scanner.Scan() {
		var issue struct {
			ID           string `json:"id"`
			Dependencies []struct {
				DependsOnID string `json:"depends_on_id"`
				Type        string `json:"type"`
			} `json:"dependencies"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &issue); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if !siblingPattern.MatchString(issue.ID) {
			continue
		}
		if seenTasks[issue.ID] {
			t.Fatalf("Beads export contains task %s more than once", issue.ID)
		}
		seenTasks[issue.ID] = true
		for _, dependency := range issue.Dependencies {
			if dependency.Type == "blocks" && siblingPattern.MatchString(dependency.DependsOnID) {
				edges = append(edges, dependencyEdge{From: dependency.DependsOnID, To: issue.ID})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for sibling := range seenTasks {
		if !taskSet[sibling] {
			t.Fatalf("Beads export has undeclared sibling issue %s", sibling)
		}
	}
	for _, edge := range edges {
		if !taskSet[edge.From] || !taskSet[edge.To] {
			t.Fatalf("Beads blocks edge %s has undeclared sibling endpoint", edge)
		}
	}
	for _, task := range tasks {
		if !seenTasks[task] {
			t.Fatalf("Beads export has no issue for task %s", task)
		}
	}
	return canonicalEdges(t, edges)
}

func compareEdges(t *testing.T, leftName string, left []dependencyEdge, rightName string, right []dependencyEdge) {
	t.Helper()
	leftSet := make(map[dependencyEdge]bool, len(left))
	rightSet := make(map[dependencyEdge]bool, len(right))
	for _, edge := range left {
		leftSet[edge] = true
	}
	for _, edge := range right {
		rightSet[edge] = true
	}
	for _, edge := range left {
		if !rightSet[edge] {
			t.Errorf("%s missing edge %s required by %s", rightName, edge, leftName)
		}
	}
	for _, edge := range right {
		if !leftSet[edge] {
			t.Errorf("%s has extra edge %s absent from %s", rightName, edge, leftName)
		}
	}
}

func canonicalEdges(t *testing.T, edges []dependencyEdge) []dependencyEdge {
	t.Helper()
	slices.SortFunc(edges, func(a, b dependencyEdge) int {
		return strings.Compare(a.String(), b.String())
	})
	for i := range len(edges) - 1 {
		if edges[i+1] == edges[i] {
			t.Fatalf("duplicate dependency edge %s", edges[i+1])
		}
	}
	return edges
}

func startText(plan string, indexes []int) string {
	return plan[indexes[0]:indexes[1]]
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("find test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}
