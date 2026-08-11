package run

// Fake is a Runner test double. It records every call and, if Func is set,
// delegates to it for the stdout/err response; otherwise it returns "", nil.
// It lives outside _test.go so config, hook, and worktree tests (and later
// gate/spawn) can all share one fake.
type Fake struct {
	// Func, if non-nil, produces the response for each call.
	Func func(dir, name string, args []string) (string, error)
	// Calls is the ordered log of invocations.
	Calls []Call
}

// Call is one recorded invocation.
type Call struct {
	Dir  string
	Name string
	Args []string
}

// Run implements Runner, recording the call before responding.
func (f *Fake) Run(dir, name string, args ...string) (string, error) {
	f.Calls = append(f.Calls, Call{Dir: dir, Name: name, Args: append([]string(nil), args...)})
	if f.Func != nil {
		return f.Func(dir, name, args)
	}
	return "", nil
}
