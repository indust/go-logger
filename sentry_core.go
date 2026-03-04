package logger

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap/zapcore"
)

const (
	defaultBreadcrumbLevel = zapcore.DebugLevel
	defaultEventLevel      = zapcore.ErrorLevel
)

// SentryUserTagMap maps field names which will be passed to sentry as User.
type SentryUserTagMap struct {
	ID        string
	IPAddress string
	Name      string
	Username  string
	Email     string
}

type SentryCore struct {
	zapcore.LevelEnabler

	hub   *sentry.Hub
	scope *sentry.Scope

	BreadcrumbLevel zapcore.Level
	EventLevel      zapcore.Level

	UserTags    SentryUserTagMap
	GenericTags []string

	// Cached from With("feature", ...) so we can build issue titles even when
	// feature isn't passed on every log call.
	featureValue string
}

type SentryCoreOption func(*SentryCore)

// BreadcrumbLevel will set a minimum level of messages should be stored as breadcrumbs.
func BreadcrumbLevel(level zapcore.Level) SentryCoreOption {
	return func(w *SentryCore) {
		w.BreadcrumbLevel = level
		w.LevelEnabler = level
		if level > w.EventLevel {
			w.EventLevel = level
		}
	}
}

// EventLevel will set a minimum level of messages should be sent as events.
func EventLevel(level zapcore.Level) SentryCoreOption {
	return func(w *SentryCore) {
		w.EventLevel = level
	}
}

// UserTags will set map to match zap fields with sentry user tags.
func UserTags(tagMap SentryUserTagMap) SentryCoreOption {
	return func(w *SentryCore) {
		w.UserTags = tagMap
	}
}

// GenericTags defines which zap fields should be passed as tags to Sentry.
func GenericTags(tags ...string) SentryCoreOption {
	return func(w *SentryCore) {
		w.GenericTags = tags
	}
}

func NewSentryCore(hub *sentry.Hub, options ...SentryCoreOption) zapcore.Core {
	if hub == nil {
		panic("hub should not be nil")
	}

	core := &SentryCore{
		LevelEnabler:    defaultBreadcrumbLevel,
		hub:             hub,
		scope:           hub.PushScope(),
		BreadcrumbLevel: defaultBreadcrumbLevel,
		EventLevel:      defaultEventLevel,
	}

	for _, option := range options {
		option(core)
	}

	return core
}

func (s *SentryCore) With(fields []zapcore.Field) zapcore.Core {
	clone := &SentryCore{
		LevelEnabler:    s.LevelEnabler,
		hub:             s.hub,
		scope:           s.hub.PushScope(),
		BreadcrumbLevel: s.BreadcrumbLevel,
		EventLevel:      s.EventLevel,
		UserTags:        s.UserTags,
		GenericTags:     s.GenericTags,
		featureValue:    s.featureValue,
	}

	data := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(data)
	}
	clone.scope.SetExtras(data.Fields)
	if v, ok := data.Fields["feature"]; ok {
		f := strings.TrimSpace(fmt.Sprint(v))
		if f == "<nil>" {
			f = ""
		}
		clone.featureValue = f
	}

	return clone
}

func (s *SentryCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if ent.Level >= s.BreadcrumbLevel {
		ce = ce.AddCore(ent, s)
	}
	return ce
}

func (s *SentryCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	data := zapcore.NewMapObjectEncoder()
	var errField error
	for _, field := range fields {
		if field.Type == zapcore.ErrorType {
			errField = field.Interface.(error) //nolint:forcetypeassert
		} else {
			field.AddTo(data)
		}
	}

	breadcrumb := sentry.Breadcrumb{
		Data:      data.Fields,
		Level:     SentryLevel(ent.Level),
		Message:   ent.Message,
		Timestamp: time.Now().UTC(),
		Type:      BreadcrumbTypeDefault,
	}
	s.hub.AddBreadcrumb(&breadcrumb, nil)

	if ent.Level >= s.EventLevel {
		s.captureEvent(ent, data, errField)
	}

	if ent.Level > zapcore.ErrorLevel {
		_ = s.Sync()
	}

	return nil
}

func (s *SentryCore) captureEvent(ent zapcore.Entry, data *zapcore.MapObjectEncoder, errField error) {
	event := sentry.NewEvent()
	event.Level = SentryLevel(ent.Level)
	event.Message = ent.Message
	s.parseFieldsToEvent(event, data.Fields)

	feature := ""
	if v, ok := data.Fields["feature"]; ok {
		feature = strings.TrimSpace(fmt.Sprint(v))
	} else {
		feature = strings.TrimSpace(s.featureValue)
	}
	if feature == "<nil>" {
		feature = ""
	}

	prefix := ""
	if ent.LoggerName != "" {
		prefix += "[" + ent.LoggerName + "]"
	}
	if feature != "" {
		prefix += "[" + feature + "]"
	}

	if errField != nil {
		//event.Exception = s.convertErrorToException(errField)
		event.SetException(errField, 12)
	}

	if len(event.Exception) != 0 {
		lastSentryException := event.Exception[len(event.Exception)-1]
		errorMessage := lastSentryException.Value
		errorSummary := strings.Split(errorMessage, ":")[0]
		logMessage := ent.Message
		if prefix != "" && !strings.HasPrefix(logMessage, prefix) {
			logMessage = prefix + logMessage
		}
		event.Message = errorMessage
		sentrySummaryRecord := sentry.Exception{
			Type:       logMessage,
			Value:      errorSummary,
			Module:     lastSentryException.Module,
			ThreadID:   lastSentryException.ThreadID,
			Stacktrace: newStacktrace(),
			Mechanism:  lastSentryException.Mechanism,
		}
		event.Exception = append(event.Exception, sentrySummaryRecord)
	} else {
		if prefix != "" && !strings.HasPrefix(event.Message, prefix) {
			event.Message = prefix + event.Message
		}
		event.Threads = []sentry.Thread{{
			ID:         "0",
			Current:    true,
			Crashed:    ent.Level >= zapcore.DPanicLevel,
			Stacktrace: newStacktrace(),
		}}
	}

	s.hub.CaptureEvent(event)
}

func (s *SentryCore) convertErrorToException(errValue error) []sentry.Exception {
	exceptions := make([]sentry.Exception, 0)
	firstMeaningfulError := -1
	for i := 0; i < 10 && errValue != nil; i++ {
		errorType := reflect.TypeOf(errValue).String()
		exceptions = append(exceptions, sentry.Exception{
			Value:      errValue.Error(),
			Type:       errorType,
			Stacktrace: extractStacktrace(errValue),
		})

		if errorType != "*fmt.wrapError" && firstMeaningfulError == -1 {
			firstMeaningfulError = i
		}

		switch wrapped := errValue.(type) { //nolint:errorlint
		case interface{ Unwrap() error }:
			errValue = wrapped.Unwrap()
		case interface{ Cause() error }:
			errValue = wrapped.Cause()
		default:
			errValue = nil
		}
	}

	// If the first errors are wrapped errors, we want to show actual error type instead of *fmt.wrapError
	if firstMeaningfulError != -1 {
		for i := 0; i < firstMeaningfulError; i++ {
			exceptions[i].Type = fmt.Sprintf("wrapped<%s>", exceptions[firstMeaningfulError].Type)
		}
	}

	return exceptions
}

func (s *SentryCore) Sync() error {
	s.hub.Flush(30 * time.Second)
	return nil
}

func (s *SentryCore) parseFieldsToEvent(event *sentry.Event, data map[string]interface{}) {
	event.User = s.prepareSentryUser(&data)
	event.Tags = s.prepareSentryTags(&data)
	event.Extra = data
}

func (s *SentryCore) prepareSentryUser(data *map[string]interface{}) sentry.User {
	return sentry.User{
		ID:        fmt.Sprintf("%v", pop(data, s.UserTags.ID)),
		IPAddress: fmt.Sprintf("%v", pop(data, s.UserTags.IPAddress)),
		Name:      fmt.Sprintf("%v", pop(data, s.UserTags.Name)),
		Username:  fmt.Sprintf("%v", pop(data, s.UserTags.Username)),
		Email:     fmt.Sprintf("%v", pop(data, s.UserTags.Email)),
	}
}

func (s *SentryCore) prepareSentryTags(data *map[string]interface{}) map[string]string {
	tags := make(map[string]string, 0)
	for _, tagKey := range s.GenericTags {
		val := fmt.Sprintf("%v", pop(data, tagKey))
		if val != "" {
			tags[tagKey] = val
		}
	}
	return tags
}

func pop(fieldMap *map[string]interface{}, key string) interface{} {
	val, ok := (*fieldMap)[key]
	if ok {
		delete(*fieldMap, key)
		return val
	}
	return ""
}
