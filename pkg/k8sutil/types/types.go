package types

type ErrorTimeout struct {
	Message string
}

func (e *ErrorTimeout) Error() string {
	return e.Message
}
