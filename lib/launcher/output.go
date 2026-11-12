package launcher

import "fmt"

// LaunchOutputLimit is how much browser output is kept and attached to a
// launch error.
var LaunchOutputLimit = 64 << 10

type limitBuf struct {
	buf []byte
	max int
}

func (b *limitBuf) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	if len(b.buf) < b.max {
		n := b.max - len(b.buf)
		if n > len(p) {
			n = len(p)
		}
		b.buf = append(b.buf, p[:n]...)
	}
	return len(p), nil
}

func (b *limitBuf) Len() int {
	if b == nil {
		return 0
	}
	return len(b.buf)
}

func (b *limitBuf) String() string {
	if b == nil {
		return ""
	}
	return string(b.buf)
}

func (l *Launcher) annotate(err error) error {
	if err == nil || l.output.Len() == 0 {
		return err
	}
	return fmt.Errorf("%w\nbrowser output:\n%s", err, l.output.String())
}
