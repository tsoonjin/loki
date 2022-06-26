package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

func parseCWEvent(ctx context.Context, ev *events.CloudwatchLogsEvent) error {
	data, err := ev.AWSLogs.Parse()
	if err != nil {
		fmt.Println("error parsing log event: ", err)
		return err
	}

	for _, event := range data.LogEvents {
        fmt.Println(event.Message)
	}

	return nil
}

func processCWEvent(ctx context.Context, ev *events.CloudwatchLogsEvent) error {
	err := parseCWEvent(ctx, ev)
	if err != nil {
		return err
	}
	return nil
}
