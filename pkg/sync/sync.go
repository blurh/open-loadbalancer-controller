package sync

type Syncer interface {
	Sync(string, ControllerResources, *RespBody) error
	Clean(string, ServiceRef) error
}

func NewSyncer(syncType string) Syncer {
	switch syncType {
	case "http":
		return NewHttpSyncer()

	case "grpc":
		return nil

	default:
		panic("unsupported sync type")
	}
}
