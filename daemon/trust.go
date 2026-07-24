package daemon

import (
	"errors"
	"fmt"

	"github.com/yasyf/daemonkit/trust"
)

// Roles names the exact authorities used by one cc-interact runtime.
type Roles struct {
	Business    trust.PeerRole
	Lifecycle   trust.PeerRole
	StopControl trust.PeerRole
}

func (r Roles) validate(policy trust.TrustPolicy) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("daemon: validate trust policy: %w", err)
	}
	if r.Business == "" || r.Lifecycle == "" || r.StopControl == "" {
		return errors.New("daemon: business, lifecycle, and stop-control roles are required")
	}
	if r.Business == trust.UnprotectedRole {
		if !policy.AllowsUnprotected() {
			return errors.New("daemon: unprotected business role is not allowed by the trust policy")
		}
	} else if _, ok := policy.Requirement(r.Business); !ok {
		return fmt.Errorf("daemon: business role %q is not declared", r.Business)
	}
	if _, ok := policy.Requirement(r.Lifecycle); !ok || !policy.AllowsReceipt(r.Lifecycle) || !policy.AllowsReadiness(r.Lifecycle) {
		return fmt.Errorf("daemon: lifecycle role %q lacks receipt and readiness authority", r.Lifecycle)
	}
	if _, ok := policy.Requirement(r.StopControl); !ok || !policy.AllowsStop(r.StopControl) {
		return fmt.Errorf("daemon: stop-control role %q lacks stop authority", r.StopControl)
	}
	return nil
}
