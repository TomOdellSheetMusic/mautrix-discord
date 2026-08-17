package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/database"
)

type Puppet struct {
	*database.Puppet

	bridge *DiscordBridge
	log    zerolog.Logger

	MXID id.UserID

	customIntent *appservice.IntentAPI
	customUser   *User

	syncLock sync.Mutex

	presenceLock  sync.Mutex
	lastPresence  event.Presence
	lastStatusMsg string
}

var _ bridge.Ghost = (*Puppet)(nil)
var _ bridge.GhostWithProfile = (*Puppet)(nil)

func (puppet *Puppet) GetMXID() id.UserID {
	return puppet.MXID
}

var userIDRegex *regexp.Regexp

func (br *DiscordBridge) NewPuppet(dbPuppet *database.Puppet) *Puppet {
	return &Puppet{
		Puppet: dbPuppet,
		bridge: br,
		log:    br.ZLog.With().Str("discord_user_id", dbPuppet.ID).Logger(),

		MXID: br.FormatPuppetMXID(dbPuppet.ID),
	}
}

func (br *DiscordBridge) ParsePuppetMXID(mxid id.UserID) (string, bool) {
	if userIDRegex == nil {
		pattern := fmt.Sprintf(
			"^@%s:%s$",
			br.Config.Bridge.FormatUsername("([0-9]+)"),
			br.Config.Homeserver.Domain,
		)

		userIDRegex = regexp.MustCompile(pattern)
	}

	match := userIDRegex.FindStringSubmatch(string(mxid))
	if len(match) == 2 {
		return match[1], true
	}

	return "", false
}

func (br *DiscordBridge) GetPuppetByMXID(mxid id.UserID) *Puppet {
	discordID, ok := br.ParsePuppetMXID(mxid)
	if !ok {
		return nil
	}

	return br.GetPuppetByID(discordID)
}

func (br *DiscordBridge) GetPuppetByID(id string) *Puppet {
	br.puppetsLock.Lock()
	defer br.puppetsLock.Unlock()

	puppet, ok := br.puppets[id]
	if !ok {
		dbPuppet := br.DB.Puppet.Get(id)
		if dbPuppet == nil {
			dbPuppet = br.DB.Puppet.New()
			dbPuppet.ID = id
			dbPuppet.Insert()
		}

		puppet = br.NewPuppet(dbPuppet)
		br.puppets[puppet.ID] = puppet
	}

	return puppet
}

func (br *DiscordBridge) GetPuppetByCustomMXID(mxid id.UserID) *Puppet {
	br.puppetsLock.Lock()
	defer br.puppetsLock.Unlock()

	puppet, ok := br.puppetsByCustomMXID[mxid]
	if !ok {
		dbPuppet := br.DB.Puppet.GetByCustomMXID(mxid)
		if dbPuppet == nil {
			return nil
		}

		puppet = br.NewPuppet(dbPuppet)
		br.puppets[puppet.ID] = puppet
		br.puppetsByCustomMXID[puppet.CustomMXID] = puppet
	}

	return puppet
}

func (br *DiscordBridge) GetAllPuppetsWithCustomMXID() []*Puppet {
	return br.dbPuppetsToPuppets(br.DB.Puppet.GetAllWithCustomMXID())
}

func (br *DiscordBridge) GetAllPuppets() []*Puppet {
	return br.dbPuppetsToPuppets(br.DB.Puppet.GetAll())
}

func (br *DiscordBridge) dbPuppetsToPuppets(dbPuppets []*database.Puppet) []*Puppet {
	br.puppetsLock.Lock()
	defer br.puppetsLock.Unlock()

	output := make([]*Puppet, len(dbPuppets))
	for index, dbPuppet := range dbPuppets {
		if dbPuppet == nil {
			continue
		}

		puppet, ok := br.puppets[dbPuppet.ID]
		if !ok {
			puppet = br.NewPuppet(dbPuppet)
			br.puppets[dbPuppet.ID] = puppet

			if dbPuppet.CustomMXID != "" {
				br.puppetsByCustomMXID[dbPuppet.CustomMXID] = puppet
			}
		}

		output[index] = puppet
	}

	return output
}

func (br *DiscordBridge) FormatPuppetMXID(did string) id.UserID {
	return id.NewUserID(
		br.Config.Bridge.FormatUsername(did),
		br.Config.Homeserver.Domain,
	)
}

func (puppet *Puppet) GetDisplayname() string {
	return puppet.Name
}

func (puppet *Puppet) GetAvatarURL() id.ContentURI {
	return puppet.AvatarURL
}

func (puppet *Puppet) DefaultIntent() *appservice.IntentAPI {
	return puppet.bridge.AS.Intent(puppet.MXID)
}

func (puppet *Puppet) IntentFor(portal *Portal) *appservice.IntentAPI {
	if puppet.customIntent == nil || (portal.Key.Receiver != "" && portal.Key.Receiver != puppet.ID) {
		return puppet.DefaultIntent()
	}

	return puppet.customIntent
}

func (puppet *Puppet) CustomIntent() *appservice.IntentAPI {
	if puppet == nil {
		return nil
	}
	return puppet.customIntent
}

func (puppet *Puppet) updatePortalMeta(meta func(portal *Portal)) {
	for _, portal := range puppet.bridge.GetDMPortalsWith(puppet.ID) {
		// Get room create lock to prevent races between receiving contact info and room creation.
		portal.roomCreateLock.Lock()
		meta(portal)
		portal.roomCreateLock.Unlock()
	}
}

func (puppet *Puppet) UpdateName(info *discordgo.User) bool {
	newName := puppet.bridge.Config.Bridge.FormatDisplayname(info, puppet.IsWebhook, puppet.IsApplication)
	if puppet.Name == newName && puppet.NameSet {
		return false
	}
	puppet.Name = newName
	puppet.NameSet = false
	err := puppet.DefaultIntent().SetDisplayName(newName)
	if err != nil {
		puppet.log.Warn().Err(err).Msg("Failed to update displayname")
	} else {
		go puppet.updatePortalMeta(func(portal *Portal) {
			if portal.UpdateNameDirect(puppet.Name, false) {
				portal.Update()
				portal.UpdateBridgeInfo()
			}
		})
		puppet.NameSet = true
	}
	return true
}

func (br *DiscordBridge) reuploadUserAvatar(intent *appservice.IntentAPI, guildID, userID, avatarID string) (id.ContentURI, string, error) {
	var downloadURL string
	if guildID == "" {
		if strings.HasPrefix(avatarID, "a_") {
			downloadURL = discordgo.EndpointUserAvatarAnimated(userID, avatarID)
		} else {
			downloadURL = discordgo.EndpointUserAvatar(userID, avatarID)
		}
	} else {
		if strings.HasPrefix(avatarID, "a_") {
			downloadURL = discordgo.EndpointGuildMemberAvatarAnimated(guildID, userID, avatarID)
		} else {
			downloadURL = discordgo.EndpointGuildMemberAvatar(guildID, userID, avatarID)
		}
	}
	url := br.DMA.AvatarMXC(guildID, userID, avatarID)
	if !url.IsEmpty() {
		return url, downloadURL, nil
	}
	copied, err := br.copyAttachmentToMatrix(intent, downloadURL, false, AttachmentMeta{
		AttachmentID: fmt.Sprintf("avatar/%s/%s/%s", guildID, userID, avatarID),
	})
	if err != nil {
		return id.ContentURI{}, downloadURL, err
	}
	return copied.MXC, downloadURL, nil
}

func (puppet *Puppet) UpdateAvatar(info *discordgo.User) bool {
	avatarID := info.Avatar
	if puppet.IsWebhook && !puppet.bridge.Config.Bridge.EnableWebhookAvatars {
		avatarID = ""
	}
	if puppet.Avatar == avatarID && puppet.AvatarSet {
		return false
	}
	avatarChanged := avatarID != puppet.Avatar
	puppet.Avatar = avatarID
	puppet.AvatarSet = false
	puppet.AvatarURL = id.ContentURI{}

	if puppet.Avatar != "" && (puppet.AvatarURL.IsEmpty() || avatarChanged) {
		url, _, err := puppet.bridge.reuploadUserAvatar(puppet.DefaultIntent(), "", info.ID, puppet.Avatar)
		if err != nil {
			puppet.log.Warn().Err(err).Str("avatar_id", puppet.Avatar).Msg("Failed to reupload user avatar")
			return true
		}
		puppet.AvatarURL = url
	}

	err := puppet.DefaultIntent().SetAvatarURL(puppet.AvatarURL)
	if err != nil {
		puppet.log.Warn().Err(err).Msg("Failed to update avatar")
	} else {
		go puppet.updatePortalMeta(func(portal *Portal) {
			if portal.UpdateAvatarFromPuppet(puppet) {
				portal.Update()
				portal.UpdateBridgeInfo()
			}
		})
		puppet.AvatarSet = true
	}
	return true
}

func (puppet *Puppet) UpdateInfo(source *User, info *discordgo.User, message *discordgo.Message) {
	puppet.syncLock.Lock()
	defer puppet.syncLock.Unlock()

	if info == nil || len(info.Username) == 0 || len(info.Discriminator) == 0 {
		if puppet.Name != "" || source == nil {
			return
		}
		var err error
		puppet.log.Debug().Str("source_user", source.DiscordID).Msg("Fetching info through user to update puppet")
		info, err = source.Session.User(puppet.ID)
		if err != nil {
			puppet.log.Error().Err(err).Str("source_user", source.DiscordID).Msg("Failed to fetch info through user")
			return
		}
	}

	err := puppet.DefaultIntent().EnsureRegistered()
	if err != nil {
		puppet.log.Error().Err(err).Msg("Failed to ensure registered")
	}

	changed := false
	if message != nil {
		if message.WebhookID != "" && message.ApplicationID == "" && !puppet.IsWebhook {
			puppet.log.Debug().
				Str("message_id", message.ID).
				Str("webhook_id", message.WebhookID).
				Msg("Found webhook ID in message, marking ghost as a webhook")
			puppet.IsWebhook = true
			changed = true
		}
		if message.ApplicationID != "" && !puppet.IsApplication {
			puppet.log.Debug().
				Str("message_id", message.ID).
				Str("application_id", message.ApplicationID).
				Msg("Found application ID in message, marking ghost as an application")
			puppet.IsApplication = true
			puppet.IsWebhook = false
			changed = true
		}
	}
	changed = puppet.UpdateContactInfo(info) || changed
	changed = puppet.UpdateName(info) || changed
	changed = puppet.UpdateAvatar(info) || changed
	if changed {
		puppet.Update()
	}
}

func (puppet *Puppet) UpdateContactInfo(info *discordgo.User) bool {
	changed := false
	if puppet.Username != info.Username {
		puppet.Username = info.Username
		changed = true
	}
	if puppet.GlobalName != info.GlobalName {
		puppet.GlobalName = info.GlobalName
		changed = true
	}
	if puppet.Discriminator != info.Discriminator {
		puppet.Discriminator = info.Discriminator
		changed = true
	}
	if puppet.IsBot != info.Bot {
		puppet.IsBot = info.Bot
		changed = true
	}
	if (changed && !puppet.IsWebhook) || !puppet.ContactInfoSet {
		puppet.ContactInfoSet = false
		puppet.ResendContactInfo()
		return true
	}
	return false
}

func (puppet *Puppet) ResendContactInfo() {
	if !puppet.bridge.SpecVersions.Supports(mautrix.BeeperFeatureArbitraryProfileMeta) || puppet.ContactInfoSet {
		return
	}
	discordUsername := puppet.Username
	if puppet.Discriminator != "0" {
		discordUsername += "#" + puppet.Discriminator
	}
	contactInfo := map[string]any{
		"com.beeper.bridge.identifiers": []string{
			fmt.Sprintf("discord:%s", discordUsername),
		},
		"com.beeper.bridge.remote_id":      puppet.ID,
		"com.beeper.bridge.service":        puppet.bridge.BeeperServiceName,
		"com.beeper.bridge.network":        puppet.bridge.BeeperNetworkName,
		"com.beeper.bridge.is_network_bot": puppet.IsBot,
	}
	if puppet.IsWebhook {
		contactInfo["com.beeper.bridge.identifiers"] = []string{}
	}
	err := puppet.DefaultIntent().BeeperUpdateProfile(contactInfo)
	if err != nil {
		puppet.log.Warn().Err(err).Msg("Failed to store custom contact info in profile")
	} else {
		puppet.ContactInfoSet = true
	}
}

const PresenceBusy event.Presence = "org.matrix.msc3026.busy"

func DiscordStatusToMatrix(status discordgo.Status) event.Presence {
	switch status {
	case discordgo.StatusOnline:
		return event.PresenceOnline
	case discordgo.StatusIdle, discordgo.StatusDoNotDisturb:
		return PresenceBusy
	case discordgo.StatusInvisible, discordgo.StatusOffline:
		return event.PresenceOffline
	default:
		return event.PresenceOffline
	}
}

func ExtractCustomStatus(presence *discordgo.Presence) string {
	if presence == nil {
		return ""
	}
	for _, activity := range presence.Activities {
		if activity != nil && activity.Type == discordgo.ActivityTypeCustom {
			return activity.State
		}
	}
	for _, activity := range presence.Activities {
		if activity == nil {
			continue
		}
		switch activity.Type {
		case discordgo.ActivityTypeGame,
			discordgo.ActivityTypeStreaming,
			discordgo.ActivityTypeListening,
			discordgo.ActivityTypeWatching,
			discordgo.ActivityTypeCompeting:
			if activity.Name != "" {
				return activity.Name
			}
		}
	}
	return ""
}

func (puppet *Puppet) SetPresence(presence event.Presence, statusMsg string) error {
	intent := puppet.DefaultIntent()
	err := intent.EnsureRegistered()
	if err != nil {
		return fmt.Errorf("failed to ensure registered: %w", err)
	}
	req := map[string]any{
		"presence":   presence,
		"status_msg": statusMsg,
	}
	url := intent.BuildClientURL("v3", "presence", puppet.MXID, "status")
	_, err = intent.MakeRequest("PUT", url, req, nil)
	return err
}

func (puppet *Puppet) UpdatePresence(presence *discordgo.Presence) {
	if presence == nil || presence.User == nil || presence.User.ID == "" {
		return
	}
	newPresence := DiscordStatusToMatrix(presence.Status)
	newStatusMsg := ExtractCustomStatus(presence)
	if newPresence == PresenceBusy && puppet.bridge.presenceBusyUnsupported.Load() {
		newPresence = event.PresenceUnavailable
	}

	puppet.presenceLock.Lock()
	defer puppet.presenceLock.Unlock()
	if newPresence == puppet.lastPresence && newStatusMsg == puppet.lastStatusMsg {
		return
	}
	puppet.lastPresence = newPresence
	puppet.lastStatusMsg = newStatusMsg

	err := puppet.SetPresence(newPresence, newStatusMsg)
	if err != nil {
		if newPresence == PresenceBusy && puppet.bridge.isInvalidPresenceError(err) {
			puppet.bridge.presenceBusyUnsupported.Store(true)
			puppet.log.Debug().Err(err).Msg("Homeserver doesn't support busy presence (MSC3026), falling back to unavailable")
			puppet.lastPresence = event.PresenceUnavailable
			if retryErr := puppet.SetPresence(event.PresenceUnavailable, newStatusMsg); retryErr != nil {
				puppet.log.Warn().Err(retryErr).Msg("Failed to update presence after falling back from busy to unavailable")
			}
		} else {
			puppet.log.Warn().Err(err).
				Str("presence", string(newPresence)).
				Str("status_msg", newStatusMsg).
				Msg("Failed to update presence")
		}
	}
}

func (br *DiscordBridge) isInvalidPresenceError(err error) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && httpErr.RespError != nil &&
		httpErr.RespError.ErrCode == "M_UNKNOWN" && httpErr.RespError.Err == "Invalid presence state"
}

func (puppet *Puppet) refreshPresence() {
	puppet.presenceLock.Lock()
	defer puppet.presenceLock.Unlock()
	if puppet.lastPresence == "" || puppet.lastPresence == event.PresenceOffline {
		return
	}
	err := puppet.SetPresence(puppet.lastPresence, puppet.lastStatusMsg)
	if err != nil {
		puppet.log.Warn().Err(err).
			Str("presence", string(puppet.lastPresence)).
			Msg("Failed to refresh presence")
	}
}
