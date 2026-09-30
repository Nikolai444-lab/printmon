package main

import "sync"

// Shutdown carries a "please stop" request from the dashboard to main().
// Веб-вкладка — это только пульт наблюдения, поэтому остановка программы
// делается отдельным явным действием, а не закрытием окна браузера.
type Shutdown struct {
	once sync.Once
	ch   chan struct{}
}

// NewShutdown creates an idle shutdown channel.
func NewShutdown() *Shutdown {
	return &Shutdown{ch: make(chan struct{})}
}

// Request asks the program to stop. Repeat calls are harmless.
func (s *Shutdown) Request() {
	s.once.Do(func() { close(s.ch) })
}

// Done is closed when a stop was requested.
func (s *Shutdown) Done() <-chan struct{} {
	return s.ch
}
