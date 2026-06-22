package model

import "time"

type BranchNode struct {
	Name       string `json:"name"`
	Ref        string `json:"ref"`
	Tip        string `json:"tip"`
	Parent     string `json:"parent,omitempty"`
	ForkPoint  string `json:"fork_point,omitempty"`
	Depth      int    `json:"depth"`
	Source     string `json:"source"`
	CommitTime string `json:"commit_time,omitempty"`
}

type BranchGraph struct {
	Version     int          `json:"version"`
	GeneratedAt string       `json:"generated_at"`
	Remote      string       `json:"remote"`
	Patterns    []string     `json:"patterns"`
	Branches    []BranchNode `json:"branches"`
}

type Evidence struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type Fact struct {
	Version       int        `json:"version"`
	Branch        string     `json:"branch"`
	Parent        string     `json:"parent,omitempty"`
	Mode          string     `json:"mode"`
	BaseCommit    string     `json:"base_commit,omitempty"`
	HeadCommit    string     `json:"head_commit"`
	BusinessLogic []string   `json:"business_logic"`
	DataFlow      []string   `json:"data_flow"`
	Evidence      []Evidence `json:"evidence"`
	GeneratedAt   string     `json:"generated_at"`
}

func NewGraph(remote string, patterns []string, branches []BranchNode) BranchGraph {
	return BranchGraph{
		Version:     1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Remote:      remote,
		Patterns:    patterns,
		Branches:    branches,
	}
}
