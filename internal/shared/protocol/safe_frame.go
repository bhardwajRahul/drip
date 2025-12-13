package protocol

import (
	"sync"
)

type SafeFrame struct {
	*Frame
	once sync.Once
}

func (sf *SafeFrame) Close() error {
	sf.once.Do(func() {
		if sf.Frame != nil {
			sf.Frame.Release()
		}
	})
	return nil
}

func WithFrame(frame *Frame) *SafeFrame {
	return &SafeFrame{Frame: frame}
}
