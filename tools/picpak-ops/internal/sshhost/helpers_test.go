package sshhost

// boolp returns a pointer to b — for the *bool config.Host.Enabled field, which is
// nil (omitted → enabled) in real configs but set explicitly in tests.
func boolp(b bool) *bool { return &b }
