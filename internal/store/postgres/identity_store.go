package postgres

// ConfigureGitHubAppKeyEncryptionKey installs the process-only AES-256 key
// that seals workspace GitHub App private keys (DEC-59 clause 2). The copy
// prevents later caller mutation and the value never enters persisted
// configuration.
func (s *Store) ConfigureGitHubAppKeyEncryptionKey(key []byte) {
	s.gitHubAppEncryptionKey = append(s.gitHubAppEncryptionKey[:0], key...)
}
