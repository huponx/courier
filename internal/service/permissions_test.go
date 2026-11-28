package service

import (
	"strings"
	"testing"

	"github.com/huponx/gocommon/grpcserver"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// Every RPC has exactly one rule and every name in them is declared in permissions.proto.
func TestCoverage(t *testing.T) {
	grpcserver.CheckCoverage(t, courierv1.File_courier_v1_courier_proto, courierv1.File_courier_v1_permissions_proto)
	grpcserver.CheckCoverage(t, courierv1.File_courier_v1_inbox_proto, courierv1.File_courier_v1_permissions_proto)
	grpcserver.CheckCoverage(t, courierv1.File_courier_v1_preferences_proto, courierv1.File_courier_v1_permissions_proto)
	grpcserver.CheckCoverage(t, courierv1.File_courier_v1_admin_proto, courierv1.File_courier_v1_permissions_proto)
	for _, sd := range []struct {
		name    string
		methods []string
	}{
		{courierv1.CourierService_ServiceDesc.ServiceName, []string{"SendNotification", "Notify", "GetNotification", "RetryDelivery"}},
		{courierv1.AdminNotificationService_ServiceDesc.ServiceName, []string{"ListEventTypes", "GetTemplate", "PreviewTemplate", "ListTemplateVersions", "SaveTemplate", "RevertTemplate", "CreateBroadcast", "ListBroadcasts", "ListDeliveries", "ListWebhooks", "SaveWebhook", "DeleteWebhook", "TestWebhook"}},
		{courierv1.PreferencesService_ServiceDesc.ServiceName, []string{"GetPreferences", "SetPreferences", "StartTelegramBinding", "UnbindTelegram", "SubscribePush", "UnsubscribePush"}},
		{courierv1.InboxService_ServiceDesc.ServiceName, []string{"ListInbox", "GetUnreadCount", "MarkRead", "Archive", "StreamInbox"}},
	} {
		for _, m := range sd.methods {
			if _, ok := Permissions["/"+sd.name+"/"+m]; !ok {
				t.Errorf("/%s/%s is not in Permissions", sd.name, m)
			}
		}
	}
}

// A user token must reach the inbox and nothing of the service API.
func TestUsersReachOnlyTheInbox(t *testing.T) {
	userPerms := []string{"dictionary:word:read"}
	for m, r := range Permissions {
		isInbox := strings.HasPrefix(m, "/courier.v1.InboxService/") || strings.HasPrefix(m, "/courier.v1.PreferencesService/")
		if got := r.Allows(userPerms); got != isInbox {
			t.Errorf("%s: a plain user allowed = %v", m, got)
		}
	}
}
