package core

const RepositoryRegistrationSource = "repository-registration"

// RepositoryInstallTask records exact onboarding membership, independently of titles.
type RepositoryInstallTask struct {
	TaskID  string    `json:"task_id"`
	Attempt int       `json:"attempt"`
	State   TaskState `json:"state"`
}
