package tuya

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tuya"
)

func Init() {
	streams.HandleFunc("tuya", func(source string) (core.Producer, error) {
		return tuya.Dial(source)
	})

	api.HandleFunc("api/tuya", apiTuya)
}

func apiTuya(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	region := query.Get("region")
	email := query.Get("email")
	password := query.Get("password")

	if email == "" || password == "" || region == "" {
		http.Error(w, "email, password and region are required", http.StatusBadRequest)
		return
	}

	var tuyaRegion *tuya.Region
	for _, r := range tuya.AvailableRegions {
		if r.Host == region {
			tuyaRegion = &r
			break
		}
	}

	if tuyaRegion == nil {
		http.Error(w, fmt.Sprintf("invalid region: %s", region), http.StatusBadRequest)
		return
	}

	tuyaAPI, err := tuya.NewTuyaSmartApiClient(tuyaRegion.Host, email, password, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err = tuyaAPI.Login(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var devices []tuya.Device

	homes, _ := tuyaAPI.GetHomeList()
	if homes != nil && len(homes.Result) > 0 {
		for _, home := range homes.Result {
			roomList, err := tuyaAPI.GetRoomList(strconv.Itoa(home.Gid))
			if err != nil {
				continue
			}

			for _, room := range roomList.Result {
				for _, device := range room.DeviceList {
					if (device.Category == "sp" || device.Category == "dghsxj") && !containsDevice(devices, device.DeviceId) {
						devices = append(devices, device)
					}
				}
			}
		}
	}

	sharedHomes, _ := tuyaAPI.GetSharedHomeList()
	if sharedHomes != nil && len(sharedHomes.Result.SecurityWebCShareInfoList) > 0 {
		for _, sharedHome := range sharedHomes.Result.SecurityWebCShareInfoList {
			for _, device := range sharedHome.DeviceInfoList {
				if (device.Category == "sp" || device.Category == "dghsxj") && !containsDevice(devices, device.DeviceId) {
					devices = append(devices, device)
				}
			}
		}
	}

	if len(devices) == 0 {
		http.Error(w, "no cameras found", http.StatusNotFound)
		return
	}

	var items []*api.Source
	for _, device := range devices {
		cleanQuery := url.Values{}
		cleanQuery.Set("device_id", device.DeviceId)
		cleanQuery.Set("email", email)
		cleanQuery.Set("password", password)
		url := fmt.Sprintf("tuya://%s?%s", tuyaRegion.Host, cleanQuery.Encode())

		items = append(items, &api.Source{
			Name: device.DeviceName,
			URL:  url,
		})
	}

	api.ResponseSources(w, items)
}

func containsDevice(devices []tuya.Device, deviceID string) bool {
	for _, device := range devices {
		if device.DeviceId == deviceID {
			return true
		}
	}
	return false
}
