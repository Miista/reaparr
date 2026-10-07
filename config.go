package main

import "github.com/robfig/cron/v3"

// cronParser accepts both standard 5-field cron expressions and the
// "@hourly"/"@daily"/etc descriptors — the poll schedule is meant to be set
// by a human, and the descriptors are the readable common case.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
