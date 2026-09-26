package alerts

// AlertLifecycle manages acknowledgement and resolution lifecycle.
type AlertLifecycle struct {
	State string
}

func (a *AlertLifecycle) Acknowledge() {
	a.State = "acknowledged"
}

func (a *AlertLifecycle) Resolve() {
	a.State = "resolved"
}
