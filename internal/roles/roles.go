// SPDX-License-Identifier: Apache-2.0

// Package roles names the factory's roles and addresses their agents, and
// holds each role's instructions for its agents.
package roles

import (
	"embed"
	"fmt"
	"slices"
	"strings"
)

// The roles.
const (
	Coordination = "coordination"
	Planning     = "planning"
	Research     = "research"
	Engineering  = "engineering"
	Quality      = "quality"
)

// All are the roles, in the order they are shown.
var All = []string{Coordination, Planning, Research, Engineering, Quality}

// Staffed are the roles M1 runs, which must be staffed for the factory to
// run: Planning and Research arrive in M3.
var Staffed = []string{Coordination, Engineering, Quality}

// Known reports whether role is a role.
func Known(role string) bool {
	return slices.Contains(All, role)
}

// Title is the role's name as it is written in prose.
func Title(role string) string {
	if role == "" {
		return ""
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

// Leader is the name of a role's leader.
const Leader = "leader"

// Address is how an agent is addressed: its name and role, written
// name@role, such as leader@coordination.
type Address struct {
	Name string
	Role string
}

// LeaderOf returns the address of role's leader.
func LeaderOf(role string) Address {
	return Address{Name: Leader, Role: role}
}

func (a Address) String() string {
	return a.Name + "@" + a.Role
}

// ParseAddress reads an agent's address: name@role, or a bare role name,
// which means its leader.
func ParseAddress(s string) (Address, error) {
	name, role, found := strings.Cut(s, "@")
	if !found {
		name, role = Leader, s
	}
	if !Known(role) {
		return Address{}, fmt.Errorf("%q names no agent: address an agent as name@role, such as leader@coordination, or by a role alone for its leader; the roles are %s", s, strings.Join(All, ", "))
	}
	if name == "" {
		return Address{}, fmt.Errorf("%q names no agent: the name before @%s is missing", s, role)
	}
	return Address{Name: name, Role: role}, nil
}

//go:embed instructions/*.md
var instructions embed.FS

// LeaderInstructions returns the instructions for role's leader.
func LeaderInstructions(role string) (string, error) {
	data, err := instructions.ReadFile("instructions/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("there are no instructions for %s's leader", Title(role))
	}
	return string(data), nil
}
