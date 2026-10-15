package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skillDirectories are the agents' skills directories, under the home
// directory, that join links the config's join.skill into.
var skillDirectories = []string{".claude/skills", ".agents/skills",
	".copilot/skills"}

// joinWithAnd lists items as "a", "a and b", or "a, b and c".
func joinWithAnd(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	last := len(items) - 1
	return strings.Join(items[:last], ", ") + " and " + items[last]
}

// shortenHome writes a path under the home directory with a leading ~.
func shortenHome(home, path string) string {
	if rest, found := strings.CutPrefix(path, home+"/"); found {
		return "~/" + rest
	}
	return path
}

// checkSkill checks that skill is a directory holding a SKILL.md.
func checkSkill(config clientConfig) error {
	info, err := os.Stat(config.joinSkill)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("join.skill in %s names %s, which is not a "+
			"directory", config.path, config.joinSkill)
	}
	_, err = os.Stat(filepath.Join(config.joinSkill, "SKILL.md"))
	if err != nil {
		return fmt.Errorf("join.skill in %s names %s, which holds no "+
			"SKILL.md", config.path, config.joinSkill)
	}
	return nil
}

// linkSkill links the skill directory into each skills directory that
// exists, under its own name, and leaves alone any other entry there.
func linkSkill(skill string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding the skills directories: %w", err)
	}
	name := filepath.Base(skill)

	var searched, found, linked, alreadyLinked, leftAlone []string
	for _, directory := range skillDirectories {
		directory = filepath.Join(home, directory)
		searched = append(searched, shortenHome(home, directory))
		_, err := os.Stat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		found = append(found, directory)

		link := filepath.Join(directory, name)
		info, err := os.Lstat(link)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Symlink(skill, link); err != nil {
				return fmt.Errorf("linking the %s skill: %w", name, err)
			}
			linked = append(linked, shortenHome(home, directory))
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(link)
			if err != nil {
				return err
			}
			if filepath.Clean(target) == filepath.Clean(skill) {
				alreadyLinked = append(alreadyLinked,
					shortenHome(home, directory))
				continue
			}
		}
		leftAlone = append(leftAlone, shortenHome(home, link))
	}

	if len(linked) > 0 {
		fmt.Printf("Linked the %s skill into %s.\n", name,
			joinWithAnd(linked))
	}
	if len(alreadyLinked) > 0 {
		fmt.Printf("The %s skill was already linked in %s.\n", name,
			joinWithAnd(alreadyLinked))
	}
	if len(found) == 0 {
		fmt.Printf("None of %s exists, so the %s skill was not linked.\n",
			joinWithAnd(searched), name)
	}
	for _, link := range leftAlone {
		fmt.Printf("Left %s alone, since it is not a link to %s.\n", link,
			skill)
	}
	return nil
}

// cmdJoin makes a workspace the user's default workspace, and links the
// config's join.skill into the agents' skills directories. With no
// name, it lists the workspaces to pick from, or joins the only one.
func cmdJoin(config clientConfig, args []string) error {
	if len(args) > 1 {
		return usageError("join [<name>]")
	}
	var name string
	if len(args) == 1 {
		name = args[0]
	} else if len(config.workspaces) == 1 {
		name = config.workspaces[0].name
	} else {
		fmt.Printf("Pick a default workspace with '%s join <name>':\n",
			programName)
		for _, workspace := range config.workspaces {
			fmt.Println("  " + workspace.name)
		}
		fmt.Println("Your agent can use any of them with --workspace <name>.")
		return nil
	}

	if _, ok := config.find(name); !ok {
		return fmt.Errorf("%s configures no workspace called %s "+
			"(configured: %s)", config.path, name, config.names())
	}
	if config.joinSkill != "" {
		if err := checkSkill(config); err != nil {
			return err
		}
	}

	path, err := settingsPath()
	if err != nil {
		return err
	}
	err = writeSettings(path, settingsFile{DefaultWorkspace: name})
	if err != nil {
		return err
	}
	fmt.Printf("Your default workspace is now %s.\n", name)
	override := os.Getenv(defaultWorkspaceEnvironment())
	if override != "" && override != name {
		fmt.Printf("%s is set to %s, which overrides this until it is "+
			"unset.\n", defaultWorkspaceEnvironment(), override)
	}
	var others []string
	for _, workspace := range config.workspaces {
		if workspace.name != name {
			others = append(others, workspace.name)
		}
	}
	if len(others) > 0 {
		fmt.Printf("Your agent can still use the other workspaces with "+
			"--workspace <name>: %s.\n", strings.Join(others, ", "))
	}

	if config.joinSkill == "" {
		return nil
	}
	return linkSkill(config.joinSkill)
}
