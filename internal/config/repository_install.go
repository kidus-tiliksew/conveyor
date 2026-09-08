package config

// InstallSwitch supplies an explicit repository installation choice.
func InstallSwitch(enabled bool) *bool { return &enabled }

// InstallEnabled defaults new registration writes to on (req-repository-onboarding AC-3.1).
func (r Repo) InstallEnabled() bool { return r.InstallConveyor == nil || *r.InstallConveyor }

// StoredRepositoryDefaults keeps legacy configuration reads off. A new write
// must resolve its default before persisting the document.
func StoredRepositoryDefaults(repos []Repo) {
	for i := range repos {
		if repos[i].InstallConveyor == nil {
			repos[i].InstallConveyor = InstallSwitch(false)
		}
	}
}
