package authn

type Onboarding struct {
	Required bool   `json:"required"`
	Step     string `json:"step"`
}

func OnboardingFor(admin Admin) Onboarding {
	step := admin.OnboardingStep
	return Onboarding{Required: step != "" && step != "complete", Step: step}
}

// SaveOnboarding changes only administrator UI progress. It cannot modify
// configuration, install games, create rooms, or start/stop a world.
func (s *Service) SaveOnboarding(adminID int, step string) (Onboarding, error) {
	switch step {
	case "deployment", "environment", "game", "room", "review", "complete":
	default:
		return Onboarding{}, ErrOnboardingStep
	}
	var admin Admin
	if err := s.admins().First(&admin, adminID).Error; err != nil {
		return Onboarding{}, err
	}
	// A delayed progress write from another tab must not reopen a finished wizard.
	if admin.OnboardingStep == "complete" {
		return OnboardingFor(admin), nil
	}
	if err := s.admins().Where("id = ? AND (onboarding_step IS NULL OR onboarding_step <> ?)", adminID, "complete").
		UpdateColumn("onboarding_step", step).Error; err != nil {
		return Onboarding{}, err
	}
	if err := s.admins().First(&admin, adminID).Error; err != nil {
		return Onboarding{}, err
	}
	return OnboardingFor(admin), nil
}
