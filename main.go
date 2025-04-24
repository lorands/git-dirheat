package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Node represents a directory or file in the repository structure (Internal)
type Node struct {
	Name     string
	Path     string // Relative path from repo root
	Value    int    // Aggregated change count
	IsFile   bool
	Children map[string]*Node
}

// JSONNode is the structure used for JSON output, compatible with D3.js
type JSONNode struct {
	Name     string      `json:"name"`
	Value    int         `json:"value"`
	Children []*JSONNode `json:"children,omitempty"` // Use slice for JSON
}

// NewNode creates a new internal Node
func NewNode(name, path string, isFile bool) *Node {
	return &Node{
		Name:     name,
		Path:     path,
		Value:    0,
		IsFile:   isFile,
		Children: make(map[string]*Node),
	}
}

// ensurePath navigates or creates nodes for the given path parts
// and returns the final node (which represents a file in this context).
func (n *Node) ensurePath(pathParts []string) *Node {
	current := n
	currentPath := "/"

	for i, part := range pathParts {
		if part == "" {
			continue // Skip empty parts
		}

		child, exists := current.Children[part]
		currentPath = filepath.Join(currentPath, part)
		isFile := (i == len(pathParts)-1) // It's a file if it's the last part

		if !exists {
			child = NewNode(part, currentPath, isFile)
			current.Children[part] = child
			// Ensure parent nodes are marked as not files if they were initially created as files
			current.IsFile = false
		}
		current = child
	}
	return current
}

// aggregateCounts recursively calculates the sum of changes for directories.
// It assumes file node values are already set.
func (n *Node) aggregateCounts() int {
	if n.IsFile {
		return n.Value // Base case: file's value is its own count
	}

	sum := 0
	for _, child := range n.Children {
		sum += child.aggregateCounts()
	}
	n.Value = sum // Set directory's value to the sum of its children
	return sum
}

// ToJSONNode converts the internal Node structure to the JSONNode structure.
func (n *Node) ToJSONNode() *JSONNode {
	jNode := &JSONNode{
		Name:  n.Name,
		Value: n.Value,
	}

	if len(n.Children) > 0 {
		jNode.Children = make([]*JSONNode, 0, len(n.Children))
		for _, child := range n.Children {
			// Only include children with changes or that are non-empty directories
			if child.Value > 0 {
				jNode.Children = append(jNode.Children, child.ToJSONNode())
			}
		}

		// Sort children by value (descending) for consistent treemap layout
		sort.Slice(jNode.Children, func(i, j int) bool {
			return jNode.Children[i].Value > jNode.Children[j].Value
		})
	}

	return jNode
}

// --- Globals ---
var (
	repoData     *Node
	dataOnce     sync.Once
	repoPath     string
	analyzeError error
)

// analyzeRepo performs the git log analysis using --numstat
func analyzeRepo(path string) (*Node, error) {
	gitDir := filepath.Join(path, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("path '%s' does not appear to be a git repository (.git directory not found)", path)
	}
	fmt.Printf("Analyzing Git repository (using numstat) at: %s", path)

	// Use --numstat to get lines added/deleted per file per commit
	cmd := exec.Command("git", "-C", path, "log", "--numstat", "--pretty=format:", "--no-merges")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// ... (Fetch/retry logic remains the same as before) ...
		log.Printf("Initial 'git log --numstat' failed. Error: %v", err)
		log.Printf("Git log output (if any):%s", string(output))
		log.Printf("Attempting git fetch --unshallow...")
		fetchCmd := exec.Command("git", "-C", path, "fetch", "--unshallow")
		fetchOutput, fetchErr := fetchCmd.CombinedOutput()
		if fetchErr != nil {
			fmt.Printf("Git fetch --unshallow failed: %v Fetch Output: %s", fetchErr, string(fetchOutput))
			fmt.Println("Attempting simple 'git fetch'...")
			fetchCmdSimple := exec.Command("git", "-C", path, "fetch")
			fetchOutputSimple, fetchErrSimple := fetchCmdSimple.CombinedOutput()
			if fetchErrSimple != nil {
				fmt.Printf("Simple 'git fetch' also failed: %v Fetch Output: %s", fetchErrSimple, string(fetchOutputSimple))
			}
		}
		fmt.Println("Retrying git log --numstat...")
		cmd = exec.Command("git", "-C", path, "log", "--numstat", "--pretty=format:", "--no-merges")
		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("Retried 'git log --numstat' failed. Error: %v", err)
			log.Printf("Git log output (after retry):%s", string(output))
			return nil, fmt.Errorf("error running git log --numstat even after fetch attempts: %v", err)
		}
		log.Println("Git log --numstat succeeded after fetch attempt.")
	}

	// --- Data Processing ---
	fileChangeCounts := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	processedLines := 0

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue // Skip empty lines between commits
		}
		processedLines++

		parts := strings.Fields(line)
		var filePath string
		var addedStr, deletedStr string
		if len(parts) < 3 {
			// Handle potential rename lines like: 1       0       src/{foo.go => bar.go} or {old/path/foo.go => new/path/bar.go}
			if strings.Contains(line, "=>") {
				// Extract the destination path robustly
				leftCurly := strings.Index(line, "{")
				rightCurly := strings.Index(line, "}")
				arrow := strings.Index(line, "=>")
				if leftCurly >= 0 && rightCurly > leftCurly && arrow > leftCurly && arrow < rightCurly {
					// e.g. src/{foo.go => bar.go}
					prefix := line[:leftCurly]
					inside := line[leftCurly+1 : rightCurly]
					insideParts := strings.Split(inside, "=>")
					if len(insideParts) == 2 {
						// Use the right side (destination)
						filePath = strings.TrimSpace(prefix + insideParts[1])
					}
				} else if leftCurly == 0 && arrow > 0 {
					// e.g. {old/path/foo.go => new/path/bar.go}
					inside := line[1:]
					arrow = strings.Index(inside, "=>")
					if arrow > 0 {
						right := inside[arrow+2:]
						right = strings.TrimPrefix(right, " ")
						right = strings.TrimSuffix(right, "}")
						filePath = strings.TrimSpace(right)
					}
				}
				// If still not found, skip
				if filePath == "" {
					log.Printf("WARN: Could not robustly parse rename line: %s", line)
					continue
				}
				if len(parts) >= 2 {
					addedStr, deletedStr = parts[0], parts[1]
				} else {
					log.Printf("WARN: Could not parse numeric fields in rename line: %s", line)
					continue
				}
			} else {
				log.Printf("WARN: Skipping malformed numstat line (expected 3+ fields): %s", line)
				continue
			}
		} else {
			// Normal line
			addedStr = parts[0]
			deletedStr = parts[1]
			filePath = parts[2]
		}

		var changeAmount int
		if addedStr == "-" || deletedStr == "-" {
			changeAmount = 1
		} else {
			changeAmount = 1
		}

		normalizedPath := filepath.ToSlash(strings.TrimSpace(filePath))
		normalizedPath = strings.TrimLeft(normalizedPath, "{ ")
		if normalizedPath != "" {
			fileChangeCounts[normalizedPath] += changeAmount
			// log.Printf("DEBUG: File: %s, Change: %d, Total: %d", normalizedPath, changeAmount, fileChangeCounts[normalizedPath]) // Verbose
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("WARN: Error reading git log output: %v", err)
		// Continue processing with data gathered so far
	}
	fmt.Printf("Processed %d numstat lines, found %d unique files changed.", processedLines, len(fileChangeCounts))

	// --- Build Tree Structure ---
	rootDirName := filepath.Base(path)
	if rootDirName == "." || rootDirName == "/" {
		rootDirName = "repository_root"
	}
	rootDir := NewNode(rootDirName, "/", false) // Root is a directory

	for filePath, count := range fileChangeCounts {
		if count == 0 {
			continue
		} // Skip files with zero count if using line changes
		pathParts := strings.Split(filePath, "/")
		// Sanitize each path segment to remove leading/trailing curly braces and whitespace
		for i, part := range pathParts {
			pathParts[i] = strings.Trim(part, " {}")
		}
		fileNode := rootDir.ensurePath(pathParts) // Create structure down to the file
		fileNode.Value = count                    // Set the file's final aggregated count
	}

	// --- Aggregate Counts Upwards ---
	log.Println("Aggregating directory counts...")
	rootDir.aggregateCounts()
	log.Printf("Aggregation complete. Root node '%s' final value: %d", rootDir.Name, rootDir.Value)

	if rootDir.Value == 0 && len(fileChangeCounts) > 0 {
		fmt.Println("Warning: Root directory value is 0 after aggregation, but files were processed. Check aggregation logic.")
	} else if rootDir.Value == 0 {
		fmt.Println("Warning: No file changes seem to have been recorded or aggregated.")
	}

	return rootDir, nil
}

// main function
func main() {
	if len(os.Args) < 2 {
		fmt.Println("Error: Missing required argument.")
		log.Fatal("Usage: go run main.go <path_to_local_git_repo>")
	}
	repoPath = os.Args[1]

	fileInfo, err := os.Stat(repoPath)
	if err != nil {
		log.Fatalf("Error accessing path '%s': %v", repoPath, err)
	}
	if !fileInfo.IsDir() {
		log.Fatalf("Path '%s' is not a directory", repoPath)
	}

	// Run analysis once
	dataOnce.Do(func() {
		log.Println("Starting initial repository analysis (numstat approach)...")
		repoData, analyzeError = analyzeRepo(repoPath)
		if analyzeError != nil {
			log.Printf("!!! CRITICAL error during initial repository analysis: %v", analyzeError)
		} else if repoData != nil {
			// Log the value calculated by aggregation now
			log.Printf("Initial repository analysis complete. Root node ('%s') aggregated value: %d", repoData.Name, repoData.Value)
		} else {
			log.Printf("Repository analysis finished, but repoData is nil (and no error reported).")
		}
	})

	http.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		if analyzeError != nil {
			log.Printf("ERROR /data: Analysis error encountered: %v", analyzeError)
			http.Error(w, fmt.Sprintf("Error analyzing repository: %v", analyzeError), http.StatusInternalServerError)
			return
		}
		if repoData == nil {
			log.Println("ERROR /data: repoData is nil")
			http.Error(w, "Repository data is not available or analysis failed.", http.StatusInternalServerError)
			return
		}

		// Convert aggregated internal structure to JSON-friendly structure
		jsonData := repoData.ToJSONNode()

		// Optional logging for the data being sent
		// log.Printf("Serving Data for Root: '%s' (Aggregated Value: %d)", jsonData.Name, jsonData.Value)
		// if len(jsonData.Children) > 0 {
		//     log.Printf("  First child: Name='%s', Value=%d", jsonData.Children[0].Name, jsonData.Children[0].Value)
		// }

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		err := json.NewEncoder(w).Encode(jsonData) // Encode the JSON-friendly structure
		if err != nil {
			log.Printf("Error encoding JSON data: %v", err)
			http.Error(w, "Error encoding JSON data", http.StatusInternalServerError)
		}
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// ... (Root handler remains the same) ...
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat("heatmap.html"); err == nil {
			http.ServeFile(w, r, "heatmap.html")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintln(w, `<!DOCTYPE html>
<html>
<head><title>Git Heatmap</title></head>
<body>
    <h1>Git Repository Heatmap</h1>
    <p><strong>Error:</strong> Could not find <code>heatmap.html</code>.</p>
    <p>Data is served at <a href="/data">/data</a>.</p>
</body>
</html>`)
		}
	})

	port := "8080"
	// ... (Server start logic remains the same) ...
	fmt.Printf("Attempting to start server on http://localhost:%s", port)
	fmt.Printf("Serving data for repository: %s", repoPath)
	fmt.Printf("Access http://localhost:%s/ for visualization (requires heatmap.html)", port)
	fmt.Printf("Access http://localhost:%s/data for raw JSON data", port)

	err = http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
