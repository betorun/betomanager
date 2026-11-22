package queue

import "go.uber.org/zap"

// asynqLogger adapts a zap.SugaredLogger to the asynq.Logger interface.
type asynqLogger struct {
	l *zap.SugaredLogger
}

func newAsynqLogger(l *zap.SugaredLogger) *asynqLogger {
	return &asynqLogger{l: l}
}

func (a *asynqLogger) Debug(args ...interface{}) { a.l.Debug(args...) }
func (a *asynqLogger) Info(args ...interface{})  { a.l.Info(args...) }
func (a *asynqLogger) Warn(args ...interface{})  { a.l.Warn(args...) }
func (a *asynqLogger) Error(args ...interface{}) { a.l.Error(args...) }
func (a *asynqLogger) Fatal(args ...interface{}) { a.l.Fatal(args...) }
