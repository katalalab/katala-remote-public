package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The roster is the fleet SSOT; getting these exclusions wrong means polling a phone
// or a retired node.
func TestLoadRosterExclusions(t *testing.T) {
	tsv := "alias\tdns\tipv4\tos\tstatus\trole\tnode_id\thost\tsession\n" +
		"win-gpu-a\td\t10.0.0.1\twindows\tonline\tgpu-dev\twin-gpu-a\tWINHOST\twindows-wsl\n" +
		"win-old-a\td\t10.0.0.2\twindows\tretired\tarchive-only\twin-old-a\tU\tnone\n" +
		"phone-a\td\t10.0.0.3\tiOS\tonline\tno-ssh-expected\tphone-a\t\tno-ssh\n" +
		"mac-a\td\t10.0.0.4\tmacOS\tonline\tprimary-mac\tmac-a\tM\tmacos\n"
	path := filepath.Join(t.TempDir(), "roster.tsv")
	if err := os.WriteFile(path, []byte(tsv), 0o600); err != nil {
		t.Fatal(err)
	}
	nodes, err := loadRoster(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 pollable nodes, got %d: %+v", len(nodes), nodes)
	}
	for _, n := range nodes {
		if n.Alias == "win-old-a" {
			t.Error("retired node must never be a polling target")
		}
		if n.Alias == "phone-a" {
			t.Error("no-ssh-expected node must never be a polling target")
		}
	}
	if nodes[0].NodeID != "win-gpu-a" || nodes[1].NodeID != "mac-a" {
		t.Errorf("node_id column misread: %+v", nodes)
	}
}

// A node with no data must render as unknown, never inherit another node's numbers -
// the "false green" hazard.
func TestUnreachableNodeStaysEmpty(t *testing.T) {
	v := NodeView{Node: Node{Alias: "n"}}
	if v.ProcdOK || v.TempMaxC != 0 || len(v.Procs) != 0 {
		t.Fatal("zero value of NodeView must not look healthy")
	}
}
