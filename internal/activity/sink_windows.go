package activity

import "errors"

func sendSyslog([]Event) error { return errors.New("there's no syslog on Windows; use a path") }
