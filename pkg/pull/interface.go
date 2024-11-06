package pull

var _p PullerInterface

func init() {
	SetPuller(&Puller{})
}

func SetPuller(p PullerInterface) {
	_p = p
}

type PullerInterface interface {
	Pull(upstreamURI string, pullOptions PullOptions) (string, error)
}

// Convenience functions

func Pull(upstreamURI string, pullOptions PullOptions) (string, error) {
	return _p.Pull(upstreamURI, pullOptions)
}
