// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var gofmtList bool

func gofmt(args []string) {
	flags.BoolVar(&gofmtList, "l", false, "list files that need to be formatted")
	flags.Parse(args)
	if len(flag.Args()) > 0 {
		fmt.Fprintf(stderr(), "Usage: %s gofmt %s [-l]\n", globalFlags, os.Args[0])
		os.Exit(2)
	}

	f := gofmtCommand
	if !gofmtList {
		f |= gofmtWrite
	}

	files, stderr := runGofmt(f)
	if gofmtList {
		w := stdout()
		for _, file := range files {
			fmt.Fprintf(w, "%s\n", file)
		}
	}
	if stderr != "" {
		dief("gofmt reported errors:\n\t%s", strings.Replace(strings.TrimSpace(stderr), "\n", "\n\t", -1))
	}
}

const (
	gofmtPreCommit = 1 << iota
	gofmtCommand
	gofmtWrite
)

// runGofmt runs the external gofmt command over modified files.
//
// The definition of "modified files" depends on the bit flags.
// If gofmtPreCommit is set, then runGofmt considers *.go files that
// differ between the index (staging area) and the branchpoint
// (the latest commit before the branch diverged from upstream).
// If gofmtCommand is set, then runGofmt considers all those files
// in addition to files with unstaged modifications.
// It never considers untracked files.
//
// As a special case for the main repo (but applied everywhere)
// *.go files under a top-level test directory are excluded from the
// formatting requirement, except those in test/bench/.
//
// If gofmtWrite is set (only with gofmtCommand, meaning this is 'git gofmt'),
// runGofmt replaces the original files with their formatted equivalents.
// Git makes this difficult. In general the file in the working tree
// (the local file system) can have unstaged changes that make it different
// from the equivalent file in the index. To help pass the precommit hook,
// 'git gofmt'  must make it easy to update the files in the index.
// One option is to run gofmt on all the files of the same name in the
// working tree and leave it to the user to sort out what should be staged
// back into the index. Another is to refuse to reformat files for which
// different versions exist in the index vs the working tree. Both of these
// options are unsatisfying: they foist busy work onto the user,
// and it's exactly the kind of busy work that a program is best for.
// Instead, when runGofmt finds files in the index that need
// reformatting, it reformats them there, bypassing the working tree.
// It also reformats files in the working tree that need reformatting.
// For both, only files modified since the branchpoint are considered.
// The result should be that both index and working tree get formatted
// correctly and diffs between the two remain meaningful (free of
// formatting distractions). Modifying files in the index directly may
// surprise Git users, but it seems the best of a set of bad choices, and
// of course those users can choose not to use 'git gofmt'.
// This is a bit more work than the other git commands do, which is
// a little worrying, but the choice being made has the nice property
// that if 'git gofmt' is interrupted, a second 'git gofmt' will put things into
// the same state the first would have.
//
// runGofmt returns a list of files that need (or needed) reformatting.
// If gofmtPreCommit is set, the names always refer to files in the index.
// If gofmtCommand is set, then a name without a suffix (see below)
// refers to both the copy in the index and the copy in the working tree
// and implies that the two copies are identical. Otherwise, in the case
// that the index and working tree differ, the file name will have an explicit
// " (staged)" or " (unstaged)" suffix saying which is meant.
//
// runGofmt also returns any standard error output from gofmt,
// usually indicating syntax errors in the Go source files.
// If gofmtCommand is set, syntax errors in index files that do not match
// the working tree show a " (staged)" suffix after the file name.
// The errors never use the " (unstaged)" suffix, in order to keep
// references to the local file system in the standard file:line form.
func runGofmt(flags int) (files []string, stderrText string) {
	pwd, err := os.Getwd()
	if err != nil {
		dief("%v", err)
	}
	pwd = filepath.Clean(pwd) // convert to host \ syntax
	if !strings.HasSuffix(pwd, string(filepath.Separator)) {
		pwd += string(filepath.Separator)
	}

	b := CurrentBranch()
	repo := repoRoot()
	if !strings.HasSuffix(repo, string(filepath.Separator)) {
		repo += string(filepath.Separator)
	}

	// Find files modified in the index compared to the branchpoint.
	indexFiles := addRoot(repo, filter(gofmtRequired, getLines("git", "diff", "--name-only", "--diff-filter=ACM", "--cached", b.Branchpoint(), "--")))
	localFiles := addRoot(repo, filter(gofmtRequired, getLines("git", "diff", "--name-only", "--diff-filter=ACM")))
	localFilesMap := stringMap(localFiles)
	isUnstaged := func(file string) bool {
		return localFilesMap[file]
	}

	if len(indexFiles) == 0 && ((flags&gofmtCommand) == 0 || len(localFiles) == 0) {
		return
	}

	// Determine which files have unstaged changes and are therefore
	// different from their index versions. For those, the index version must
	// be copied into a temporary file in the local file system.
	needTemp := filter(isUnstaged, indexFiles)

	// Map between temporary file name and place in file tree where
	// file would be checked out (if not for the unstaged changes).
	tempToFile := map[string]string{}
	fileToTemp := map[string]string{}
	cleanup := func() {
		for temp := range tempToFile {
			os.Remove(temp)
		}
		tempToFile = nil
	}
	defer cleanup() // harmless if tempToFile is empty
	// Ask Git to copy the index versions into temporary files.
	// Git stores the temporary files, named .merge_*, in the repo root.
	for _, file := range needTemp {
		for _, line := range getLines("git", "checkout-index", "--temp", "--", file) {
			i := strings.Index(line, "\t")
			if i < 0 {
				continue
			}
			temp := line[:i]
			temp = filepath.Join(repo, temp)
			tempToFile[temp] = file
			fileToTemp[file] = temp
		}
	}
	dief := func(format string, args ...interface{}) {
		cleanup()
		dief(format, args...) // calling top-level dief function
	}

	// Run gofmt to find out which files need reformatting;
	// if gofmtWrite is set, reformat them in place.
	// For references to local files, remove leading pwd if present
	// to make relative to current directory.
	// Temp files and local-only files stay as absolute paths for easy matching in output.
	args := []string{"-l"}
	if flags&gofmtWrite != 0 {
		args = append(args, "-w")
	}
	for _, file := range indexFiles {
		if isUnstaged(file) {
			args = append(args, fileToTemp[file])
		} else {
			args = append(args, strings.TrimPrefix(file, pwd))
		}
	}
	if flags&gofmtCommand != 0 {
		for _, file := range localFiles {
			args = append(args, file)
		}
	}

	cmd := exec.Command("gofmt", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()

	if stderr.Len() == 0 && err != nil {
		// Error but no stderr: usually can't find gofmt.
		dief("invoking gofmt: %v", err)
	}

	// Build file list.
	files = strings.Split(stdout.String(), "\n")
	if len(files) > 0 && files[len(files)-1] == "" {
		files = files[:len(files)-1]
	}

	// Restage files that need to be restaged.
	if flags&gofmtWrite != 0 {
		add := []string{"add"}
		write := []string{"hash-object", "-w", "--"}
		updateIndex := []string{}
		for _, file := range files {
			if real := tempToFile[file]; real != "" {
				write = append(write, file)
				updateIndex = append(updateIndex, strings.TrimPrefix(real, repo))
			} else if !isUnstaged(file) {
				add = append(add, file)
			}
		}
		if len(add) > 1 {
			run("git", add...)
		}
		if len(updateIndex) > 0 {
			hashes := getLines("git", write...)
			if len(hashes) != len(write)-3 {
				dief("git hash-object -w did not write expected number of objects")
			}
			var buf bytes.Buffer
			for i, name := range updateIndex {
				fmt.Fprintf(&buf, "100644 %s\t%s\n", hashes[i], name)
			}
			cmd := exec.Command("git", "update-index", "--index-info")
			cmd.Stdin = &buf
			out, err := cmd.CombinedOutput()
			if err != nil {
				dief("git update-index: %v\n%s", err, out)
			}
		}
	}

	// Remap temp files back to original names for caller.
	for i, file := range files {
		if real := tempToFile[file]; real != "" {
			if flags&gofmtCommand != 0 {
				real += " (staged)"
			}
			files[i] = strings.TrimPrefix(real, pwd)
		} else if isUnstaged(file) {
			files[i] = strings.TrimPrefix(file+" (unstaged)", pwd)
		}
	}

	// Rewrite temp names in stderr, and shorten local file names.
	// No suffix added for local file names (see comment above).
	text := "\n" + stderr.String()
	for temp, file := range tempToFile {
		if flags&gofmtCommand != 0 {
			file += " (staged)"
		}
		text = strings.Replace(text, "\n"+temp+":", "\n"+strings.TrimPrefix(file, pwd)+":", -1)
	}
	for _, file := range localFiles {
		text = strings.Replace(text, "\n"+file+":", "\n"+strings.TrimPrefix(file, pwd)+":", -1)
	}
	text = text[1:]

	sort.Strings(files)
	return files, text
}

// gofmtRequired reports whether the specified file should be checked
// for gofmt'dness by the pre-commit hook.
// The file name is relative to the repo root.
func gofmtRequired(file string) bool {
	return strings.HasSuffix(file, ".go") &&
		!(strings.HasPrefix(file, "test/") && !strings.HasPrefix(file, "test/bench/"))
}

// stringMap returns a map m such that m[s] == true if s was in the original list.
func stringMap(list []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range list {
		m[x] = true
	}
	return m
}

// filter returns the elements in list satisfying f.
func filter(f func(string) bool, list []string) []string {
	var out []string
	for _, x := range list {
		if f(x) {
			out = append(out, x)
		}
	}
	return out
}

func addRoot(root string, list []string) []string {
	var out []string
	for _, x := range list {
		out = append(out, filepath.Join(root, x))
	}
	return out
}
