//go:build !linux && !darwin

package discover

// Local 在 linux / darwin 之外的平台不可用。
func Local() ([]Candidate, error) {
	return nil, ErrUnsupported
}
