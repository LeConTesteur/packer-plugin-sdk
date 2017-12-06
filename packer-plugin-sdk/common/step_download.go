package common

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/packer/packer"
	"github.com/mitchellh/multistep"

	"gopkg.in/cheggaaa/pb.v1"
)

// StepDownload downloads a remote file using the download client within
// this package. This step handles setting up the download configuration,
// progress reporting, interrupt handling, etc.
//
// Uses:
//   cache packer.Cache
//   ui    packer.Ui
type StepDownload struct {
	// The checksum and the type of the checksum for the download
	Checksum     string
	ChecksumType string

	// A short description of the type of download being done. Example:
	// "ISO" or "Guest Additions"
	Description string

	// The name of the key where the final path of the ISO will be put
	// into the state.
	ResultKey string

	// The path where the result should go, otherwise it goes to the
	// cache directory.
	TargetPath string

	// A list of URLs to attempt to download this thing.
	Url []string

	// Extension is the extension to force for the file that is downloaded.
	// Some systems require a certain extension. If this isn't set, the
	// extension on the URL is used. Otherwise, this will be forced
	// on the downloaded file for every URL.
	Extension string
}

func (s *StepDownload) Run(state multistep.StateBag) multistep.StepAction {
	cache := state.Get("cache").(packer.Cache)
	ui := state.Get("ui").(packer.Ui)

	var checksum []byte
	if s.Checksum != "" {
		var err error
		checksum, err = hex.DecodeString(s.Checksum)
		if err != nil {
			state.Put("error", fmt.Errorf("Error parsing checksum: %s", err))
			return multistep.ActionHalt
		}
	}

	ui.Say(fmt.Sprintf("Downloading or copying %s", s.Description))

	// First try to use any already downloaded file
	// If it fails, proceed to regualar download logic

	var downloadConfigs = make([]*DownloadConfig, len(s.Url))
	var finalPath string
	for i, url := range s.Url {
		targetPath := s.TargetPath
		if targetPath == "" {
			// Determine a cache key. This is normally just the URL but
			// if we force a certain extension we hash the URL and add
			// the extension to force it.
			cacheKey := url
			if s.Extension != "" {
				hash := sha1.Sum([]byte(url))
				cacheKey = fmt.Sprintf(
					"%s.%s", hex.EncodeToString(hash[:]), s.Extension)
			}

			log.Printf("Acquiring lock to download: %s", url)
			targetPath = cache.Lock(cacheKey)
			defer cache.Unlock(cacheKey)
		}

		config := &DownloadConfig{
			Url:        url,
			TargetPath: targetPath,
			CopyFile:   false,
			Hash:       HashForType(s.ChecksumType),
			Checksum:   checksum,
			UserAgent:  "Packer",
		}
		downloadConfigs[i] = config

		if match, _ := NewDownloadClient(config).VerifyChecksum(config.TargetPath); match {
			ui.Message(fmt.Sprintf("Found already downloaded, initial checksum matched, no download needed: %s", url))
			finalPath = config.TargetPath
			break
		}
	}

	if finalPath == "" {
		for i, url := range s.Url {
			ui.Message(fmt.Sprintf("Downloading or copying: %s", url))

			config := downloadConfigs[i]

			path, err, retry := s.download(config, state)
			if err != nil {
				ui.Message(fmt.Sprintf("Error downloading: %s", err))
			}

			if !retry {
				return multistep.ActionHalt
			}

			if err == nil {
				finalPath = path
				break
			}
		}
	}

	if finalPath == "" {
		err := fmt.Errorf("%s download failed.", s.Description)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	state.Put(s.ResultKey, finalPath)
	return multistep.ActionContinue
}

func (s *StepDownload) Cleanup(multistep.StateBag) {}

func (s *StepDownload) download(config *DownloadConfig, state multistep.StateBag) (string, error, bool) {
	var path string
	ui := state.Get("ui").(packer.Ui)

	// design the appearance of the progress bar
	bar := pb.New64(0)
	bar.ShowPercent = true
	bar.ShowCounters = true
	bar.ShowSpeed = false
	bar.ShowBar = true
	bar.ShowTimeLeft = false
	bar.ShowFinalTime = false
	bar.SetUnits(pb.U_BYTES)
	bar.Format("[=>-]")
	bar.SetRefreshRate(1 * time.Second)
	bar.SetWidth(25)
	bar.Callback = ui.Message

	// create download client with config and progress bar
	download := NewDownloadClient(config, bar)

	downloadCompleteCh := make(chan error, 1)
	go func() {
		var err error
		path, err = download.Get()
		downloadCompleteCh <- err
	}()

	for {
		select {
		case err := <-downloadCompleteCh:
			bar.Finish()

			if err != nil {
				return "", err, true
			}
			return path, nil, true

		case <-time.After(1 * time.Second):
			if _, ok := state.GetOk(multistep.StateCancelled); ok {
				bar.Finish()
				ui.Say("Interrupt received. Cancelling download...")
				return "", nil, false
			}
		}
	}
}
