package condition

import (
	"context"
	"fmt"
)

// Result is returned by each Check call.
type Result struct {
	Status CheckStatus
	Detail string
	Err    error
}

type CheckStatus string

const (
	CheckSatisfied   CheckStatus = "satisfied"
	CheckUnsatisfied CheckStatus = "unsatisfied"
	CheckFatal       CheckStatus = "fatal"
)

type Descriptor struct {
	Backend string
	Target  string
	Name    string
}

type Role string

const (
	RoleReady Role = "ready"
	RoleGuard Role = "guard"
)

func (d Descriptor) DisplayName() string {
	if d.Name != "" {
		return d.Name
	}
	if d.Backend != "" && d.Target != "" {
		return d.Backend + " " + d.Target
	}
	if d.Backend != "" {
		return d.Backend
	}
	return d.Target
}

// Condition is the single interface all backends implement. Check must return
// promptly when ctx is cancelled; the runner does not forcibly interrupt checks
// that ignore context.
type Condition interface {
	Descriptor() Descriptor
	Check(ctx context.Context) Result
}

type RoleProvider interface {
	ConditionRole() Role
}

type GuardCondition struct {
	Inner Condition
}

func NewGuard(inner Condition) *GuardCondition {
	return &GuardCondition{Inner: inner}
}

func (c *GuardCondition) ConditionRole() Role {
	return RoleGuard
}

func (c *GuardCondition) Descriptor() Descriptor {
	if c.Inner == nil {
		return Descriptor{Backend: "guard", Target: "", Name: "guard <nil>"}
	}
	desc := c.Inner.Descriptor()
	desc.Name = "guard " + desc.DisplayName()
	return desc
}

func (c *GuardCondition) Check(ctx context.Context) Result {
	if c.Inner == nil {
		return Fatal(fmt.Errorf("guard condition is required"))
	}
	result := c.Inner.Check(ctx)
	if result.Status == CheckSatisfied {
		detail := "guard condition satisfied"
		if result.Detail != "" {
			detail += ": " + result.Detail
		}
		return FatalDetail(detail, fmt.Errorf("guard condition satisfied"))
	}
	return result
}

func Satisfied(detail string) Result {
	return Result{Status: CheckSatisfied, Detail: detail}
}

func Unsatisfied(detail string, err error) Result {
	return Result{Status: CheckUnsatisfied, Detail: detail, Err: err}
}

func Fatal(err error) Result {
	return Result{Status: CheckFatal, Err: err}
}

func FatalDetail(detail string, err error) Result {
	return Result{Status: CheckFatal, Detail: detail, Err: err}
}
