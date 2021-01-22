/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2020 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"log"
	"os"
)

func (device *Device) Debugf(msg string, args ...interface{}) {
	if device.log.Debug != nil {
		device.log.Debug.Printf(msg, args...)
	}
}

func (device *Device) Debug(args ...interface{}) {
	if device.log.Debug != nil {
		device.log.Debug.Println(args...)
	}
}

func (device *Device) Infof(msg string, args ...interface{}) {
	if device.log.Info != nil {
		device.log.Info.Printf(msg, args...)
	}
}

func (device *Device) Info(args ...interface{}) {
	if device.log.Info != nil {
		device.log.Info.Println(args...)
	}
}

func (device *Device) Errorf(msg string, args ...interface{}) {
	if device.log.Error != nil {
		device.log.Error.Printf(msg, args...)
	}
}

func (device *Device) Error(args ...interface{}) {
	if device.log.Error != nil {
		device.log.Error.Println(args...)
	}
}

const (
	LogLevelSilent = iota
	LogLevelError
	LogLevelInfo
	LogLevelDebug
)

type Logger struct {
	Debug *log.Logger
	Info  *log.Logger
	Error *log.Logger
}

func NewLogger(level int, prepend string) *Logger {
	logger := new(Logger)
	if level >= LogLevelDebug {
		logger.Debug = log.New(os.Stdout,
			"DEBUG: "+prepend,
			log.Ldate|log.Ltime,
		)
	}
	if level >= LogLevelInfo {
		logger.Info = log.New(os.Stdout,
			"INFO: "+prepend,
			log.Ldate|log.Ltime,
		)
	}
	if level >= LogLevelError {
		logger.Error = log.New(os.Stdout,
			"ERROR: "+prepend,
			log.Ldate|log.Ltime,
		)
	}
	return logger
}
