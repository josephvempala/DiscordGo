package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kkdai/youtube/v2"
	"layeh.com/gopus"
)

var MusicPlayerInstance = &MusicPlayer{
	make(map[string]*GuildPlayer),
	0,
	sync.Mutex{},
	nil,
}

type MusicPlayer struct {
	GuildPlayers      map[string]*GuildPlayer
	Count             uint64
	mu                sync.Mutex
	discordConnection *discordgo.Session
}
type GuildPlayer struct {
	vc              *discordgo.VoiceConnection
	vcMx            sync.Mutex
	members         []string
	queue           []string
	status          int
	statusCondition *sync.Cond
	ytClient        youtube.Client
}

const (
	MP_STATUS_IDLE = iota
	MP_STATUS_PLAYING
	MP_STATUS_STOPPED
	MP_STATUS_ENDED
)

func main() {
	token := os.Getenv("TOKEN")
	if token == "" {
		fmt.Println("No token provided. Please provide a token in the TOKEN environment variable.")
		return
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		fmt.Println("Error creating Discord session: ", err)
		return
	}
	dg.AddHandler(messageCreate)
	dg.AddHandler(onVoiceStateUpdate)
	dg.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildVoiceStates |
		discordgo.IntentsGuildMembers
	err = dg.Open()
	if err != nil {
		fmt.Println("Error opening Discord session: ", err)
	}
	MusicPlayerInstance.discordConnection = dg
	fmt.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	dg.Close()
}

func onVoiceStateUpdate(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	g, ok := MusicPlayerInstance.GuildPlayers[vs.GuildID]
	if !ok {
		return
	}
	if vs.Member.User.Bot {
		// if the bot is issuing commands to itself do nothing
		if vs.Member.User.ID != s.State.User.ID {
			return
		}
		// if bot has been forcibly disconnected from the voice channel, clear the associated guild player
		if g.vc != nil && vs.ChannelID != g.vc.ChannelID && vs.ChannelID == "" {
			g.ClearGuildPlayer()
		}
		return
	}
	// if the user is not in the voice channel before the update and is in the voice channel after the update
	if (vs.BeforeUpdate == nil && vs.ChannelID == g.vc.ChannelID) ||
		(vs.BeforeUpdate.ChannelID != g.vc.ChannelID && vs.ChannelID == g.vc.ChannelID) {
		index, found := slices.BinarySearch(g.members, vs.UserID)
		if !found {
			g.vcMx.Lock()
			g.members = slices.Insert(g.members, index, vs.UserID)
			g.vcMx.Unlock()
			g.OnUserJoinVoiceChannel()
		}
	}
	// if the user is in the voice channel before the update and is not in the voice channel after the update
	if vs.BeforeUpdate != nil && vs.BeforeUpdate.ChannelID == g.vc.ChannelID && vs.ChannelID != g.vc.ChannelID {
		for i, member := range g.members {
			if member == vs.UserID {
				g.vcMx.Lock()
				g.members = slices.Delete(g.members, i, i+1)
				g.vcMx.Unlock()
				break
			}
		}
		g.OnUserLeaveVoiceChannel()
	}
}

func (g *GuildPlayer) OnUserJoinVoiceChannel() {
	if g.vc == nil {
		return
	}
	if len(g.members) == 1 {
		g.vc.Speaking(true)
		g.SetPlayerState(MP_STATUS_PLAYING)
	}
}

func (g *GuildPlayer) OnUserLeaveVoiceChannel() {
	if g.vc == nil {
		return
	}
	if len(g.members) == 0 {
		g.vc.Speaking(false)
		g.SetPlayerState(MP_STATUS_STOPPED)
	}
}

func (g *GuildPlayer) SetPlayerState(state int) {
	g.vcMx.Lock()
	defer g.vcMx.Unlock()
	if g.vc == nil {
		return
	}
	g.status = state
	g.statusCondition.Broadcast()
	if state == MP_STATUS_STOPPED {
		timer := time.NewTimer(5 * time.Minute)
		go func() {
			<-timer.C
			if g.status == MP_STATUS_STOPPED {
				g.SetPlayerState(MP_STATUS_ENDED)
			}
		}()
	}
}

func (g *GuildPlayer) ClearGuildPlayer() {
	g.vcMx.Lock()
	defer g.vcMx.Unlock()
	if g.vc != nil {
		g.vc.Disconnect()
		g.vc.Close()
	}
	MusicPlayerInstance.mu.Lock()
	delete(MusicPlayerInstance.GuildPlayers, g.vc.GuildID)
	MusicPlayerInstance.Count--
	if MusicPlayerInstance.Count == 0 {
		MusicPlayerInstance.discordConnection.Close()
		MusicPlayerInstance.mu.Unlock()
		g.vcMx.Unlock()
		os.Exit(1)
	}
	MusicPlayerInstance.mu.Unlock()
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if strings.HasPrefix(m.Content, "-p") {
		c, err := s.State.Channel(m.ChannelID)
		if err != nil {
			return
		}
		g, err := s.State.Guild(c.GuildID)
		if err != nil {
			return
		}
		for _, vs := range g.VoiceStates {
			if vs.UserID == m.Author.ID {
				id, err := GetYoutubeVideoID(m.Content[3:])
				if err != nil {
					fmt.Println("Error getting video ID: ", err)
					s.ChannelMessageSendReply(m.ChannelID, "Error getting video ID", m.Reference())
					return
				}
				guild, ok := MusicPlayerInstance.GuildPlayers[g.ID]
				if ok {
					// if the user is not in the same voice channel as the bot
					if guild.vc != nil && vs.ChannelID != guild.vc.ChannelID {
						s.ChannelMessageSendReply(m.ChannelID, "You need to be in the same voice channel as the bot to use this command.", m.Reference())
						return
					}
					guild.vcMx.Lock()
					guild.queue = append(guild.queue, id)
					guild.vcMx.Unlock()
					guild.SetPlayerState(MP_STATUS_PLAYING)
					return
				}
				members := []string{}
				for _, guildVcMembers := range g.VoiceStates {
					// if voice channel is that of bot and user is not the bot
					if guildVcMembers.ChannelID == vs.ChannelID && guildVcMembers.UserID != s.State.User.ID {
						members = append(members, guildVcMembers.UserID)
					}
				}
				mp := &GuildPlayer{
					queue:    []string{id},
					ytClient: youtube.Client{},
					vcMx:     sync.Mutex{},
					members:  members,
					status:   MP_STATUS_IDLE,
				}
				mp.statusCondition = sync.NewCond(&mp.vcMx)
				MusicPlayerInstance.mu.Lock()
				MusicPlayerInstance.GuildPlayers[g.ID] = mp
				MusicPlayerInstance.Count++
				MusicPlayerInstance.mu.Unlock()
				go MusicPlayerInstance.GuildPlayers[g.ID].StartPlayer(s, g.ID, vs.ChannelID, m.ChannelID)
				return
			}
		}
		s.ChannelMessageSendReply(m.ChannelID, "You need to be in a voice channel to use this command. ", m.Reference())
	}
	if strings.HasPrefix(m.Content, "-s") {
		c, err := s.State.Channel(m.ChannelID)
		if err != nil {
			return
		}
		g, err := s.State.Guild(c.GuildID)
		if err != nil {
			return
		}
		if guild, ok := MusicPlayerInstance.GuildPlayers[g.ID]; ok {
			if guild.status == MP_STATUS_PLAYING {
				guild.SetPlayerState(MP_STATUS_STOPPED)
				return
			}
			guild.SetPlayerState(MP_STATUS_PLAYING)
			return
		}
		s.ChannelMessageSendReply(m.ChannelID, "No audio is currently playing in this guild.", m.Reference())
	}
}

func (g *GuildPlayer) StartPlayer(s *discordgo.Session, guildID, voiceChannel string, messageChannel string) (err error) {
	g.vcMx.Lock()
	g.vc, err = s.ChannelVoiceJoin(guildID, voiceChannel, false, true)
	defer g.ClearGuildPlayer()
	if err != nil {
		g.vcMx.Unlock()
		return err
	}
	g.vcMx.Unlock()
	for len(g.queue) > 0 {
		err = g.playSound(s, messageChannel)
		if err != nil {
			fmt.Println("Error playing sound: in StartPlayer", err)
			return err
		}
		if len(g.queue) == 0 {
			g.SetPlayerState(MP_STATUS_STOPPED)
			g.vcMx.Lock()
			for g.status != MP_STATUS_PLAYING {
				g.statusCondition.Wait()
				if g.status == MP_STATUS_ENDED {
					g.vcMx.Unlock()
					return
				}
			}
			g.vcMx.Unlock()
		}
	}
	return nil
}

func (g *GuildPlayer) playSound(s *discordgo.Session, channelID string) (err error) {
	video, err := g.ytClient.GetVideo(g.queue[0])
	if err != nil {
		return err
	}
	formats := video.Formats.Select(func(f youtube.Format) bool {
		return strings.HasPrefix(f.MimeType, "audio")
	})
	sort.Slice(formats, func(i, j int) bool {
		return formats[i].ItagNo > formats[j].ItagNo
	})
	run := exec.Command("ffmpeg", "-i", "pipe:0", "-analyzeduration", "0", "-loglevel", "0", "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
	stream, _, err := g.ytClient.GetStream(video, &formats[0])
	if err != nil {
		fmt.Println("Error getting audio stream: ", err)
		s.ChannelMessageSend(channelID, "Error getting audio stream")
		return err
	}
	defer stream.Close()
	run.Stdin = stream
	ffmpegout, err := run.StdoutPipe()
	if err != nil {
		fmt.Println("Error receiving converted audio stream ", err)
		s.ChannelMessageSend(channelID, "Error while receiving converted audio stream")
		return err
	}
	defer ffmpegout.Close()
	ffmpegbuf := bufio.NewReaderSize(ffmpegout, 16384)
	err = run.Start()
	if err != nil {
		fmt.Println("Error starting FFmpeg: ", err)
		s.ChannelMessageSend(channelID, "Error while starting stream transformer")
		panic(err)
	}
	defer run.Process.Kill()
	g.vc.Speaking(true)
	defer g.vc.Speaking(false)
	g.SetPlayerState(MP_STATUS_PLAYING)
	defer g.SetPlayerState(MP_STATUS_STOPPED)
	send := make(chan []int16, 960*2)
	finishedPlaying := make(chan struct{})
	go func() {
		g.SendPCM(g.vc, send)
		finishedPlaying <- struct{}{}
	}()
	s.ChannelMessageSend(channelID, fmt.Sprintf("Now playing: %s", video.Title))
	defer func() {
		if err := recover(); err != nil {
			if erro, ok := err.(error); ok && erro.Error() == "send on closed channel" {
				return
			}
			fmt.Println("Error playing sound: ", err)
			s.ChannelMessageSend(channelID, "Error playing sound")
		}
		g.queue = g.queue[1:]
	}()
	for {
		audiobuf := make([]int16, 960*2)
		err = binary.Read(ffmpegbuf, binary.LittleEndian, &audiobuf)
		if err != nil {
			if err.Error() == "EOF" || err.Error() == "unexpected EOF" {
				send <- nil
				err = nil
				break
			}
			send <- nil
			break
		}
		select {
		case <-finishedPlaying:
			return nil
		case send <- audiobuf:
		}
	}
	<-finishedPlaying
	return nil
}

func (g *GuildPlayer) SendPCM(v *discordgo.VoiceConnection, pcm <-chan []int16) {
	if pcm == nil {
		return
	}
	opusEncoder, err := gopus.NewEncoder(48000, 2, gopus.Audio)
	if err != nil {
		fmt.Println("Error creating opus encoder: ", err)
		return
	}
	for {
		g.vcMx.Lock()
		for g.status != MP_STATUS_PLAYING {
			if g.status == MP_STATUS_ENDED || g.status == MP_STATUS_STOPPED {
				g.vcMx.Unlock()
				return
			}
			g.statusCondition.Wait()
			if g.status == MP_STATUS_ENDED || g.status == MP_STATUS_STOPPED {
				g.vcMx.Unlock()
				return
			}
		}
		g.vcMx.Unlock()
		recv, ok := <-pcm
		if !ok || recv == nil {
			return
		}
		opus, err := opusEncoder.Encode(recv, 960, 960*2)
		if err != nil {
			fmt.Println("Error encoding PCM: ", err)
			return
		}
		if !v.Ready || v.OpusSend == nil {
			return
		}
		v.OpusSend <- opus
	}
}

func GetYoutubeVideoID(query string) (string, error) {
	if strings.Contains(query, "youtube.com") {
		regexp := regexp.MustCompile(`(?:v=)(.{11})`)
		res := regexp.FindAllStringSubmatch(query, 1)
		if len(res) > 0 {
			return res[0][1], nil
		}
	}
	result, err := http.Get("https://www.youtube.com/results?search_query=" + url.QueryEscape(query))
	if err != nil {
		return "", err
	}
	resBytes, err := io.ReadAll(result.Body)
	if err != nil {
		return "", err
	}
	regexp := regexp.MustCompile(`(?:\[{"v.*?:")(.{11})`)
	res := regexp.FindAllSubmatch(resBytes, 1)
	if len(res) > 0 {
		return string(res[0][1]), nil
	}
	return "", nil
}
