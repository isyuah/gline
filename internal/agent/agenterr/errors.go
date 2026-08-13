package agenterr

type ErrorKind uint8

const (
	ErrorKindUnknown ErrorKind = iota
	ErrorKindTemporary
	ErrorKindFatal
)
