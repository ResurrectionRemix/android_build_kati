// Copyright 2015 Google Inc. All rights reserved
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kati

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
)

type nodeState int

const (
	nodeInit  nodeState = iota // not visited
	nodeVisit                  // visited
	nodeFile                   // visited & file exists
	nodeAlias                  // visited & alias for other target
	nodeBuild                  // visited & build emitted
)

func (s nodeState) String() string {
	switch s {
	case nodeInit:
		return "node-init"
	case nodeVisit:
		return "node-visit"
	case nodeFile:
		return "node-file"
	case nodeAlias:
		return "node-alias"
	case nodeBuild:
		return "node-build"
	default:
		return fmt.Sprintf("node-unknown[%d]", int(s))
	}
}

// NinjaGenerator generates ninja build files from DepGraph.
type NinjaGenerator struct {
	// Args is original arguments to generate the ninja file.
	Args []string
	// Suffix is suffix for generated files.
	Suffix string
	// GomaDir is goma directory.  If empty, goma will not be used.
	GomaDir string
	// DetectAndroidEcho detects echo as description.
	DetectAndroidEcho bool
	// ErrorOnEnvChange cause error when env change is detected when run ninja.
	ErrorOnEnvChange bool

	f       *os.File
	nodes   []*DepNode
	exports map[string]bool

	ctx *execContext

	ruleID     int
	done       map[string]nodeState
	shortNames map[string][]string
}

func (n *NinjaGenerator) init(g *DepGraph) {
	n.nodes = g.nodes
	n.exports = g.exports
	n.ctx = newExecContext(g.vars, g.vpaths, true)
	n.done = make(map[string]nodeState)
	n.shortNames = make(map[string][]string)
}

func getDepfileImpl(ss string) (string, error) {
	tss := ss + " "
	if (!strings.Contains(tss, " -MD ") && !strings.Contains(tss, " -MMD ")) || !strings.Contains(tss, " -c ") {
		return "", nil
	}

	mfIndex := strings.Index(ss, " -MF ")
	if mfIndex >= 0 {
		mf := trimLeftSpace(ss[mfIndex+4:])
		if strings.Index(mf, " -MF ") >= 0 {
			return "", fmt.Errorf("Multiple output file candidates in %s", ss)
		}
		mfEndIndex := strings.IndexAny(mf, " \t\n")
		if mfEndIndex >= 0 {
			mf = mf[:mfEndIndex]
		}

		return mf, nil
	}

	outIndex := strings.Index(ss, " -o ")
	if outIndex < 0 {
		return "", fmt.Errorf("Cannot find the depfile in %s", ss)
	}
	out := trimLeftSpace(ss[outIndex+4:])
	if strings.Index(out, " -o ") >= 0 {
		return "", fmt.Errorf("Multiple output file candidates in %s", ss)
	}
	outEndIndex := strings.IndexAny(out, " \t\n")
	if outEndIndex >= 0 {
		out = out[:outEndIndex]
	}
	return stripExt(out) + ".d", nil
}

// getDepfile gets depfile from cmdline, and returns cmdline and depfile.
func getDepfile(cmdline string) (string, string, error) {
	// A hack for Android - llvm-rs-cc seems not to emit a dep file.
	if strings.Contains(cmdline, "bin/llvm-rs-cc ") {
		return cmdline, "", nil
	}

	depfile, err := getDepfileImpl(cmdline)
	if depfile == "" || err != nil {
		return cmdline, depfile, err
	}

	// A hack for Makefiles generated by automake.
	mvCmd := "(mv -f " + depfile + " "
	if i := strings.LastIndex(cmdline, mvCmd); i >= 0 {
		rest := cmdline[i+len(mvCmd):]
		ei := strings.IndexByte(rest, ')')
		if ei < 0 {
			return cmdline, "", fmt.Errorf("unbalanced parenthes? %s", cmdline)
		}
		cmdline = cmdline[:i] + "(cp -f " + depfile + " " + rest
		return cmdline, depfile, nil
	}

	// A hack for Android to get .P files instead of .d.
	p := stripExt(depfile) + ".P"
	if strings.Contains(cmdline, p) {
		rmfCmd := "; rm -f " + depfile
		ncmdline := strings.Replace(cmdline, rmfCmd, "", 1)
		if ncmdline == cmdline {
			return cmdline, "", fmt.Errorf("cannot find removal of .d file: %s", cmdline)
		}
		return ncmdline, p, nil
	}

	// A hack for Android. For .s files, GCC does not use
	// C preprocessor, so it ignores -MF flag.
	as := "/" + stripExt(filepath.Base(depfile)) + ".s"
	if strings.Contains(cmdline, as) {
		return cmdline, "", nil
	}

	cmdline += fmt.Sprintf(" && cp %s %s.tmp", depfile, depfile)
	depfile += ".tmp"
	return cmdline, depfile, nil
}

func stripShellComment(s string) string {
	if strings.IndexByte(s, '#') < 0 {
		// Fast path.
		return s
	}
	// set space as an initial value so the leading comment will be
	// stripped out.
	lastch := rune(' ')
	var escape bool
	var quote rune
	for i, c := range s {
		if quote != 0 {
			if quote == c && (quote == '\'' || !escape) {
				quote = 0
			}
		} else if !escape {
			if c == '#' && isWhitespace(lastch) {
				return s[:i]
			} else if c == '\'' || c == '"' || c == '`' {
				quote = c
			}
		}
		if escape {
			escape = false
		} else if c == '\\' {
			escape = true
		} else {
			escape = false
		}
		lastch = c
	}
	return s
}

var ccRE = regexp.MustCompile(`^prebuilts/(gcc|clang)/.*(gcc|g\+\+|clang|clang\+\+) .* ?-c `)

func gomaCmdForAndroidCompileCmd(cmd string) (string, bool) {
	i := strings.Index(cmd, " ")
	if i < 0 {
		return cmd, false
	}
	driver := cmd[:i]
	if strings.HasSuffix(driver, "ccache") {
		return gomaCmdForAndroidCompileCmd(cmd[i+1:])
	}
	return cmd, ccRE.MatchString(cmd)
}

func descriptionFromCmd(cmd string) (string, bool) {
	if !strings.HasPrefix(cmd, "echo") || !isWhitespace(rune(cmd[4])) {
		return "", false
	}
	echoarg := cmd[5:]

	// strip outer quotes, and fail if it is not a single echo command.
	var buf bytes.Buffer
	var escape bool
	var quote rune
	for _, c := range echoarg {
		if escape {
			escape = false
			buf.WriteRune(c)
			continue
		}
		if c == '\\' {
			escape = true
			buf.WriteRune(c)
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			buf.WriteRune(c)
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '<', '>', '&', '|', ';':
			return "", false
		default:
			buf.WriteRune(c)
		}
	}
	return buf.String(), true
}

func (n *NinjaGenerator) genShellScript(runners []runner) (cmd string, desc string, useLocalPool bool) {
	const defaultDesc = "build $out"
	var useGomacc bool
	var buf bytes.Buffer
	for i, r := range runners {
		if i > 0 {
			if runners[i-1].ignoreError {
				buf.WriteString(" ; ")
			} else {
				buf.WriteString(" && ")
			}
		}
		cmd := stripShellComment(r.cmd)
		cmd = trimLeftSpace(cmd)
		cmd = strings.Replace(cmd, "\\\n\t", "", -1)
		cmd = strings.Replace(cmd, "\\\n", "", -1)
		cmd = strings.TrimRight(cmd, " \t\n;")
		cmd = escapeNinja(cmd)
		if cmd == "" {
			cmd = "true"
		}
		glog.V(2).Infof("cmd %q=>%q", r.cmd, cmd)
		if n.GomaDir != "" {
			rcmd, ok := gomaCmdForAndroidCompileCmd(cmd)
			if ok {
				cmd = fmt.Sprintf("%s/gomacc %s", n.GomaDir, rcmd)
				useGomacc = true
			}
		}
		if n.DetectAndroidEcho && desc == "" {
			d, ok := descriptionFromCmd(cmd)
			if ok {
				desc = d
				cmd = "true"
			}
		}
		needsSubShell := i > 0 || len(runners) > 1
		if cmd[0] == '(' {
			needsSubShell = false
		}

		if needsSubShell {
			buf.WriteByte('(')
		}
		buf.WriteString(cmd)
		if i == len(runners)-1 && r.ignoreError {
			buf.WriteString(" ; true")
		}
		if needsSubShell {
			buf.WriteByte(')')
		}
	}
	if desc == "" {
		desc = defaultDesc
	}
	return buf.String(), desc, n.GomaDir != "" && !useGomacc
}

func (n *NinjaGenerator) genRuleName() string {
	ruleName := fmt.Sprintf("rule%d", n.ruleID)
	n.ruleID++
	return ruleName
}

func (n *NinjaGenerator) emitBuild(output, rule, inputs, orderOnlys string) {
	fmt.Fprintf(n.f, "build %s: %s", escapeBuildTarget(output), rule)
	if inputs != "" {
		fmt.Fprintf(n.f, " %s", inputs)
	}
	if orderOnlys != "" {
		fmt.Fprintf(n.f, " || %s", orderOnlys)
	}
}

func escapeBuildTarget(s string) string {
	i := strings.IndexAny(s, "$: ")
	if i < 0 {
		return s
	}
	var buf bytes.Buffer
	for _, c := range s {
		switch c {
		case '$', ':', ' ':
			buf.WriteByte('$')
		}
		buf.WriteRune(c)
	}
	return buf.String()
}

func getDepString(node *DepNode) (string, string) {
	var deps []string
	seen := make(map[string]bool)
	for _, d := range node.Deps {
		t := escapeBuildTarget(d.Output)
		if seen[t] {
			continue
		}
		deps = append(deps, t)
		seen[t] = true
	}
	var orderOnlys []string
	for _, d := range node.OrderOnlys {
		t := escapeBuildTarget(d.Output)
		if seen[t] {
			continue
		}
		orderOnlys = append(orderOnlys, t)
		seen[t] = true
	}
	return strings.Join(deps, " "), strings.Join(orderOnlys, " ")
}

func escapeNinja(s string) string {
	return strings.Replace(s, "$", "$$", -1)
}

func escapeShell(s string) string {
	i := strings.IndexAny(s, "$`!\\\"")
	if i < 0 {
		return s
	}
	var buf bytes.Buffer
	var lastDollar bool
	for _, c := range s {
		switch c {
		case '$':
			if lastDollar {
				buf.WriteRune(c)
				lastDollar = false
				continue
			}
			buf.WriteString(`\$`)
			lastDollar = true
			continue
		case '`', '"', '!', '\\':
			buf.WriteByte('\\')
		}
		buf.WriteRune(c)
		lastDollar = false
	}
	return buf.String()
}

func (n *NinjaGenerator) ninjaVars(s string, nv [][]string, esc func(string) string) string {
	for _, v := range nv {
		k, v := v[0], v[1]
		if v == "" {
			continue
		}
		if strings.Contains(v, "/./") || strings.Contains(v, "/../") || strings.Contains(v, "$") {
			// ninja will normalize paths (/./, /../), so keep it as is
			// ninja will emit quoted string for $
			continue
		}
		if esc != nil {
			v = esc(v)
		}
		s = strings.Replace(s, v, k, -1)
	}
	return s
}

func (n *NinjaGenerator) emitNode(node *DepNode) error {
	if _, found := n.done[node.Output]; found {
		return nil
	}
	n.done[node.Output] = nodeVisit

	if len(node.Cmds) == 0 && len(node.Deps) == 0 && len(node.OrderOnlys) == 0 && !node.IsPhony {
		if _, ok := n.ctx.vpaths.exists(node.Output); ok {
			n.done[node.Output] = nodeFile
			return nil
		}
		o := filepath.Clean(node.Output)
		if o != node.Output {
			// if normalized target has been done, it marks as alias.
			if s, found := n.done[o]; found {
				glog.V(1).Infof("node %s=%s => %s=alias", o, s, node.Output)
				n.done[node.Output] = nodeAlias
				return nil
			}
		}
		return nil
	}

	base := filepath.Base(node.Output)
	if base != node.Output {
		n.shortNames[base] = append(n.shortNames[base], node.Output)
	}

	runners, _, err := createRunners(n.ctx, node)
	if err != nil {
		return err
	}
	ruleName := "phony"
	useLocalPool := false
	inputs, orderOnlys := getDepString(node)
	if len(runners) > 0 {
		ruleName = n.genRuleName()
		fmt.Fprintf(n.f, "\n# rule for %q\n", node.Output)
		fmt.Fprintf(n.f, "rule %s\n", ruleName)

		ss, desc, ulp := n.genShellScript(runners)
		if ulp {
			useLocalPool = true
		}
		fmt.Fprintf(n.f, " description = %s\n", desc)
		cmdline, depfile, err := getDepfile(ss)
		if err != nil {
			return err
		}
		if depfile != "" {
			fmt.Fprintf(n.f, " depfile = %s\n", depfile)
			fmt.Fprintf(n.f, " deps = gcc\n")
		}
		nv := [][]string{
			[]string{"${in}", inputs},
			[]string{"${out}", escapeNinja(node.Output)},
		}
		// It seems Linux is OK with ~130kB.
		// TODO: Find this number automatically.
		ArgLenLimit := 100 * 1000
		if len(cmdline) > ArgLenLimit {
			fmt.Fprintf(n.f, " rspfile = $out.rsp\n")
			cmdline = n.ninjaVars(cmdline, nv, nil)
			fmt.Fprintf(n.f, " rspfile_content = %s\n", cmdline)
			fmt.Fprintf(n.f, " command = %s $out.rsp\n", n.ctx.shell)
		} else {
			cmdline = escapeShell(cmdline)
			cmdline = n.ninjaVars(cmdline, nv, escapeShell)
			fmt.Fprintf(n.f, " command = %s -c \"%s\"\n", n.ctx.shell, cmdline)
		}
	}
	n.emitBuild(node.Output, ruleName, inputs, orderOnlys)
	if useLocalPool {
		fmt.Fprintf(n.f, " pool = local_pool\n")
	}
	fmt.Fprintf(n.f, "\n")
	n.done[node.Output] = nodeBuild

	for _, d := range node.Deps {
		err := n.emitNode(d)
		if err != nil {
			return err
		}
		glog.V(1).Infof("node %s dep node %q %s", node.Output, d.Output, n.done[d.Output])
	}
	for _, d := range node.OrderOnlys {
		err := n.emitNode(d)
		if err != nil {
			return err
		}
		glog.V(1).Infof("node %s order node %q %s", node.Output, d.Output, n.done[d.Output])
	}
	return nil
}

func (n *NinjaGenerator) emitRegenRules() error {
	if len(n.Args) == 0 {
		return nil
	}
	mkfiles, err := n.ctx.ev.EvaluateVar("MAKEFILE_LIST")
	if err != nil {
		return err
	}
	fmt.Fprintf(n.f, `
rule regen_ninja
 description = Regenerate ninja files due to dependency
 generator=1
 command=%s
`, strings.Join(n.Args, " "))
	fmt.Fprintf(n.f, "build %s: regen_ninja %s", n.ninjaName(), mkfiles)
	// TODO: Add dependencies to directories read by $(wildcard) or
	// $(shell find).
	if len(usedEnvs) > 0 {
		fmt.Fprintf(n.f, " %s", n.envlistName())
	}
	fmt.Fprintf(n.f, "\n\n")
	if len(usedEnvs) == 0 {
		return nil
	}
	fmt.Fprint(n.f, `
build .always_build: phony
rule regen_envlist
 description = Check $out
 generator = 1
 restat = 1
 command = rm -f $out.tmp`)
	for env := range usedEnvs {
		fmt.Fprintf(n.f, " && echo %s=$$%s >> $out.tmp", env, env)
	}
	if n.ErrorOnEnvChange {
		fmt.Fprintln(n.f, " && (cmp -s $out.tmp $out || (echo Environment variable changes are detected && diff -u $out $out.tmp))")
	} else {
		fmt.Fprintln(n.f, " && (cmp -s $out.tmp $out || mv $out.tmp $out)")
	}

	fmt.Fprintf(n.f, "build %s: regen_envlist .always_build\n\n", n.envlistName())
	return nil
}

func (n *NinjaGenerator) shName() string {
	return fmt.Sprintf("ninja%s.sh", n.Suffix)
}

func (n *NinjaGenerator) ninjaName() string {
	return fmt.Sprintf("build%s.ninja", n.Suffix)
}

func (n *NinjaGenerator) envlistName() string {
	return fmt.Sprintf(".kati_env%s", n.Suffix)
}

func (n *NinjaGenerator) generateEnvlist() (err error) {
	f, err := os.Create(n.envlistName())
	if err != nil {
		return err
	}
	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
	}()
	for k := range usedEnvs {
		v, err := n.ctx.ev.EvaluateVar(k)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "%q=%q\n", k, v)
	}
	return nil
}

func (n *NinjaGenerator) generateShell() (err error) {
	f, err := os.Create(n.shName())
	if err != nil {
		return err
	}
	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
	}()

	fmt.Fprintf(f, "#!/bin/bash\n")
	fmt.Fprintf(f, "# Generated by kati %s\n", gitVersion)
	fmt.Fprintln(f)
	fmt.Fprintln(f, `cd $(dirname "$0")`)
	if n.Suffix != "" {
		fmt.Fprintf(f, "if [ -f %s ]; then\n export $(cat %s)\nfi\n", n.envlistName(), n.envlistName())
	}
	for name, export := range n.exports {
		// export "a b"=c will error on bash
		// bash: export `a b=c': not a valid identifier
		if strings.ContainsAny(name, " \t\n\r") {
			glog.V(1).Infof("ignore export %q (export:%t)", name, export)
			continue
		}
		if export {
			v, err := n.ctx.ev.EvaluateVar(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(f, "export %q=%q\n", name, v)
		} else {
			fmt.Fprintf(f, "unset %q\n", name)
		}
	}
	if n.GomaDir == "" {
		fmt.Fprintf(f, `exec ninja -f %s "$@"`+"\n", n.ninjaName())
	} else {
		fmt.Fprintf(f, `exec ninja -f %s -j500 "$@"`+"\n", n.ninjaName())
	}

	return f.Chmod(0755)
}

func (n *NinjaGenerator) generateNinja(defaultTarget string) (err error) {
	f, err := os.Create(n.ninjaName())
	if err != nil {
		return err
	}
	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
	}()

	n.f = f
	fmt.Fprintf(n.f, "# Generated by kati %s\n", gitVersion)
	fmt.Fprintf(n.f, "\n")

	if len(usedEnvs) > 0 {
		fmt.Fprintln(n.f, "# Environment variables used:")
		var names []string
		for name := range usedEnvs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v, err := n.ctx.ev.EvaluateVar(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(n.f, "# %q=%q\n", name, v)
		}
		fmt.Fprintf(n.f, "\n")
	}

	if n.GomaDir != "" {
		fmt.Fprintf(n.f, "pool local_pool\n")
		fmt.Fprintf(n.f, " depth = %d\n\n", runtime.NumCPU())
	}

	err = n.emitRegenRules()
	if err != nil {
		return err
	}

	// defining $out for $@ and $in for $^ here doesn't work well,
	// because these texts will be processed in escapeShell...
	for _, node := range n.nodes {
		err := n.emitNode(node)
		if err != nil {
			return err
		}
		glog.V(1).Infof("node %q %s", node.Output, n.done[node.Output])
	}

	// emit phony targets for visited nodes that are
	//  - not existing file
	//  - not alias for other targets.
	var nodes []string
	for node, state := range n.done {
		if state != nodeVisit {
			continue
		}
		nodes = append(nodes, node)
	}
	if len(nodes) > 0 {
		fmt.Fprintln(n.f)
		sort.Strings(nodes)
		for _, node := range nodes {
			n.emitBuild(node, "phony", "", "")
			fmt.Fprintln(n.f)
			n.done[node] = nodeBuild
		}
	}

	// emit default if the target was emitted.
	if defaultTarget != "" && n.done[defaultTarget] == nodeBuild {
		fmt.Fprintf(n.f, "\ndefault %s\n", escapeNinja(defaultTarget))
	}

	var names []string
	for name := range n.shortNames {
		if n.done[name] != nodeInit {
			continue
		}
		if len(n.shortNames[name]) != 1 {
			// we generate shortcuts only for targets whose basename are unique.
			continue
		}
		names = append(names, name)
	}
	if len(names) > 0 {
		fmt.Fprintf(n.f, "\n# shortcuts:\n")
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(n.f, "build %s: phony %s\n", name, n.shortNames[name][0])
		}
	}
	return nil
}

// Save generates build.ninja from DepGraph.
func (n *NinjaGenerator) Save(g *DepGraph, name string, targets []string) error {
	startTime := time.Now()
	n.init(g)
	err := n.generateEnvlist()
	if err != nil {
		return err
	}
	err = n.generateShell()
	if err != nil {
		return err
	}
	var defaultTarget string
	if len(targets) == 0 && len(g.nodes) > 0 {
		defaultTarget = g.nodes[0].Output
	}
	err = n.generateNinja(defaultTarget)
	if err != nil {
		return err
	}
	logStats("generate ninja time: %q", time.Since(startTime))
	return nil
}
