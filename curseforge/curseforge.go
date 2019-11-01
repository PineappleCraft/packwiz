package curseforge

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/comp500/packwiz/cmd"
	"github.com/comp500/packwiz/core"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/cobra"
)

var curseforgeCmd = &cobra.Command{
	Use:     "curseforge",
	Aliases: []string{"cf", "curse"},
	Short:   "Manage curseforge-based mods",
}

func init() {
	cmd.Add(curseforgeCmd)
	core.Updaters["curseforge"] = cfUpdater{}
}

var fileIDRegexes = [...]*regexp.Regexp{
	regexp.MustCompile("^https?://minecraft\\.curseforge\\.com/projects/(.+)/files/(\\d+)"),
	regexp.MustCompile("^https?://(?:www\\.)?curseforge\\.com/minecraft/mc-mods/(.+)/files/(\\d+)"),
	regexp.MustCompile("^https?://(?:www\\.)?curseforge\\.com/minecraft/mc-mods/(.+)/download/(\\d+)"),
}

func getFileIDsFromString(mod string) (bool, int, int, error) {
	for _, v := range fileIDRegexes {
		matches := v.FindStringSubmatch(mod)
		if matches != nil && len(matches) == 3 {
			modID, err := modIDFromSlug(matches[1])
			fileID, err := strconv.Atoi(matches[2])
			if err != nil {
				return true, 0, 0, err
			}
			return true, modID, fileID, nil
		}
	}
	return false, 0, 0, nil
}

var modSlugRegexes = [...]*regexp.Regexp{
	regexp.MustCompile("^https?://minecraft\\.curseforge\\.com/projects/([^/]+)"),
	regexp.MustCompile("^https?://(?:www\\.)?curseforge\\.com/minecraft/mc-mods/([^/]+)"),
	// Exact slug matcher
	regexp.MustCompile("^[a-z][\\da-z\\-_]{0,127}$"),
}

func getModIDFromString(mod string) (bool, int, error) {
	// Check if it's just a number first
	modID, err := strconv.Atoi(mod)
	if err == nil && modID > 0 {
		return true, modID, nil
	}

	for _, v := range modSlugRegexes {
		matches := v.FindStringSubmatch(mod)
		if matches != nil {
			var slug string
			if len(matches) == 2 {
				slug = matches[1]
			} else if len(matches) == 1 {
				slug = matches[0]
			} else {
				continue
			}
			modID, err := modIDFromSlug(slug)
			if err != nil {
				return true, 0, err
			}
			return true, modID, nil
		}
	}
	return false, 0, nil
}

func createModFile(modInfo modInfo, fileInfo modFileInfo, index *core.Index) error {
	updateMap := make(map[string]map[string]interface{})
	var err error

	updateMap["curseforge"], err = cfUpdateData{
		ProjectID: modInfo.ID,
		FileID:    fileInfo.ID,
		// TODO: determine update channel
		ReleaseChannel: "beta",
	}.ToMap()
	if err != nil {
		return err
	}

	modMeta := core.Mod{
		Name:     modInfo.Name,
		FileName: fileInfo.FileName,
		Side:     core.UniversalSide,
		Download: core.ModDownload{
			URL: fileInfo.DownloadURL,
			// TODO: murmur2 hashing may be unstable in curse api, calculate the hash manually?
			// TODO: check if the hash is invalid (e.g. 0)
			HashFormat: "murmur2",
			Hash:       strconv.Itoa(fileInfo.Fingerprint),
		},
		Update: updateMap,
	}
	path := modMeta.SetMetaName(modInfo.Slug)

	// If the file already exists, this will overwrite it!!!
	// TODO: Should this be improved?
	// Current strategy is to go ahead and do stuff without asking, with the assumption that you are using
	// VCS anyway.

	format, hash, err := modMeta.Write()
	if err != nil {
		return err
	}

	return index.RefreshFileWithHash(path, format, hash, true)
}

type cfUpdateData struct {
	ProjectID      int    `mapstructure:"project-id"`
	FileID         int    `mapstructure:"file-id"`
	ReleaseChannel string `mapstructure:"release-channel"`
}

func (u cfUpdateData) ToMap() (map[string]interface{}, error) {
	newMap := make(map[string]interface{})
	err := mapstructure.Decode(u, &newMap)
	return newMap, err
}

type cfUpdater struct{}

func (u cfUpdater) ParseUpdate(updateUnparsed map[string]interface{}) (interface{}, error) {
	var updateData cfUpdateData
	err := mapstructure.Decode(updateUnparsed, &updateData)
	return updateData, err
}

type cachedStateStore struct {
	modInfo
	hasFileInfo bool
	fileID      int
	fileInfo    modFileInfo
}

func (u cfUpdater) CheckUpdate(mods []core.Mod, mcVersion string) ([]core.UpdateCheck, error) {
	results := make([]core.UpdateCheck, len(mods))
	modIDs := make([]int, len(mods))
	modInfos := make([]modInfo, len(mods))

	for i, v := range mods {
		projectRaw, ok := v.GetParsedUpdateData("curseforge")
		if !ok {
			results[i] = core.UpdateCheck{Error: errors.New("couldn't parse mod data")}
			continue
		}
		project := projectRaw.(cfUpdateData)
		modIDs[i] = project.ProjectID
	}

	modInfosUnsorted, err := getModInfoMultiple(modIDs)
	if err != nil {
		return nil, err
	}
	for _, v := range modInfosUnsorted {
		for i, id := range modIDs {
			if id == v.ID {
				modInfos[i] = v
				break
			}
		}
	}

	for i, v := range mods {
		projectRaw, ok := v.GetParsedUpdateData("curseforge")
		if !ok {
			results[i] = core.UpdateCheck{Error: errors.New("couldn't parse mod data")}
			continue
		}
		project := projectRaw.(cfUpdateData)

		updateAvailable := false
		fileID := project.FileID
		fileInfoObtained := false
		var fileInfoData modFileInfo
		var fileName string

		for _, file := range modInfos[i].GameVersionLatestFiles {
			// TODO: change to timestamp-based comparison??
			// TODO: manage alpha/beta/release correctly, check update channel?
			// Choose "newest" version by largest ID
			if file.GameVersion == mcVersion && file.ID > fileID {
				updateAvailable = true
				fileID = file.ID
				fileName = file.Name
			}
		}

		if !updateAvailable {
			results[i] = core.UpdateCheck{UpdateAvailable: false}
			continue
		}

		// The API also provides some files inline, because that's efficient!
		for _, file := range modInfos[i].LatestFiles {
			if file.ID == fileID {
				fileInfoObtained = true
				fileInfoData = file
			}
		}

		results[i] = core.UpdateCheck{
			UpdateAvailable: true,
			UpdateString:    v.FileName + " -> " + fileName,
			CachedState:     cachedStateStore{modInfos[i], fileInfoObtained, fileID, fileInfoData},
		}
	}
	return results, nil
}

func (u cfUpdater) DoUpdate(mods []*core.Mod, cachedState []interface{}) error {
	// "Do" isn't really that accurate, more like "Apply", because all the work is done in CheckUpdate!
	for i, v := range mods {
		modState := cachedState[i].(cachedStateStore)

		fileInfoData := modState.fileInfo
		if !modState.hasFileInfo {
			var err error
			fileInfoData, err = getFileInfo(modState.ID, modState.fileID)
			if err != nil {
				return err
			}
		}

		v.FileName = fileInfoData.FileName
		v.Name = modState.Name
		v.Download = core.ModDownload{
			URL: fileInfoData.DownloadURL,
			// TODO: murmur2 hashing may be unstable in curse api, calculate the hash manually?
			// TODO: check if the hash is invalid (e.g. 0)
			HashFormat: "murmur2",
			Hash:       strconv.Itoa(fileInfoData.Fingerprint),
		}

		v.Update["curseforge"]["project-id"] = modState.ID
		v.Update["curseforge"]["file-id"] = fileInfoData.ID
	}

	return nil
}
