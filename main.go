package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
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
}

type MusicPlayer struct {
	GuildPlayers map[string]*GuildPlayer
}
type VoiceUpdateBroker struct {
	listeners []chan string
	mx        sync.RWMutex
}

type GuildPlayer struct {
	vc             *discordgo.VoiceConnection
	queue          []string
	ytClient       youtube.Client
	members        []string
	vcUpdateBroker VoiceUpdateBroker
	mx             sync.RWMutex
}

func (v *VoiceUpdateBroker) ClearListeners() {
	defer func() {
		if err := recover(); err != nil {
			if erro, ok := err.(error); ok && erro.Error() == "close of closed channel" {
				return
			}
		}
	}()
	for _, l := range v.listeners {
		close(l)
		return
	}
}

func (v *VoiceUpdateBroker) BroadcastVoiceUpdate(update string) {
	v.mx.Lock()
	defer v.mx.Unlock()
	for _, listener := range v.listeners {
		go func(listener chan string) {
			select {
			case listener <- update:
			case <-time.After(1 * time.Second):
				return
			}
		}(listener)
	}
}

func (v *VoiceUpdateBroker) GetVoiceUpdateListener() <-chan string {
	v.mx.Lock()
	defer v.mx.Unlock()
	listener := make(chan string)
	v.listeners = append(v.listeners, listener)
	return listener
}

func (v *VoiceUpdateBroker) ClearVoiceUpdateListener(listener <-chan string) {
	defer func() {
		if err := recover(); err != nil {
			if erro, ok := err.(error); ok && erro.Error() == "send on closed channel" {
				return
			}
		}
	}()
	v.mx.Lock()
	defer v.mx.Unlock()
	for i, l := range v.listeners {
		if l == listener {
			v.listeners = append(v.listeners[:i], v.listeners[i+1:]...)
			close(l)
			return
		}
	}
}

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
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildVoiceStates
	err = dg.Open()
	if err != nil {
		fmt.Println("Error opening Discord session: ", err)
	}
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
		return
	}
	// if the user is not in the voice channel before the update and is in the voice channel after the update
	if (vs.BeforeUpdate == nil && vs.ChannelID == g.vc.ChannelID) ||
		(vs.BeforeUpdate.ChannelID != g.vc.ChannelID && vs.ChannelID == g.vc.ChannelID) {
		sort.Slice(g.members, func(i, j int) bool {
			return g.members[i] < g.members[j]
		})
		_, found := sort.Find(len(g.members), func(i int) int {
			if g.members[i] == vs.UserID {
				return 0
			}
			return 1
		})
		if !found {
			g.mx.Lock()
			g.members = append(g.members, vs.UserID)
			g.mx.Unlock()
			if len(g.members) > 0 {
				g.vcUpdateBroker.BroadcastVoiceUpdate("User joined voice channel")
			}
		}
	}
	// if the user is in the voice channel before the update and is not in the voice channel after the update
	if vs.BeforeUpdate != nil && vs.BeforeUpdate.ChannelID == g.vc.ChannelID && vs.ChannelID != g.vc.ChannelID {
		for i, member := range g.members {
			if member == vs.UserID {
				g.mx.Lock()
				g.members = append(g.members[:i], g.members[i+1:]...)
				g.mx.Unlock()
				break
			}
		}
		if len(g.members) <= 0 {
			g.vcUpdateBroker.BroadcastVoiceUpdate("No members in voice channel")
		}
	}
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
					guild.mx.Lock()
					guild.queue = append(guild.queue, id)
					guild.mx.Unlock()
					guild.vcUpdateBroker.BroadcastVoiceUpdate("User requested play")
					return
				}
				MusicPlayerInstance.GuildPlayers[g.ID] = &GuildPlayer{
					queue:    []string{id},
					ytClient: youtube.Client{},
					mx:       sync.RWMutex{},
					vcUpdateBroker: struct {
						listeners []chan string
						mx        sync.RWMutex
					}{
						listeners: make([]chan string, 0),
						mx:        sync.RWMutex{},
					},
				}
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
			guild.vcUpdateBroker.BroadcastVoiceUpdate("User requested stop")
			return
		}
		s.ChannelMessageSendReply(m.ChannelID, "No audio is currently playing in this guild.", m.Reference())
	}
}

func (g *GuildPlayer) StartPlayer(s *discordgo.Session, guildID, voiceChannel string, messageChannel string) (err error) {
	g.mx.Lock()
	g.vc, err = s.ChannelVoiceJoin(guildID, voiceChannel, false, true)
	defer func() {
		g.mx.Lock()
		g.vc.Disconnect()
		g.vc.Close()
		delete(MusicPlayerInstance.GuildPlayers, guildID)
		g.vcUpdateBroker.ClearListeners()
		g.mx.Unlock()
	}()
	if err != nil {
		g.mx.Unlock()
		return err
	}
	g.mx.Unlock()
	for len(g.queue) > 0 {
		err = g.playSound(s, messageChannel)
		if err != nil {
			fmt.Println("Error playing sound: in StartPlayer", err)
			return err
		}
		if len(g.queue) == 0 && g.vc.Ready {
			i := true
			updateListener := g.vcUpdateBroker.GetVoiceUpdateListener()
			for i {
				select {
				case update := <-updateListener:
					if update == "User requested play" {
						i = false
					}
				case <-time.After(1 * time.Minute):
					g.vcUpdateBroker.ClearVoiceUpdateListener(updateListener)
					return
				}
			}
			g.vcUpdateBroker.ClearVoiceUpdateListener(updateListener)
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
	run := exec.Command("ffmpeg", "-i", "pipe:0", "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
	stream, _, err := g.ytClient.GetStream(video, &formats[0])
	if err != nil {
		fmt.Println("Error getting audio stream: ", err)
		go s.ChannelMessageSend(channelID, "Error getting audio stream")
		return err
	}
	defer stream.Close()
	run.Stdin = stream
	ffmpegout, err := run.StdoutPipe()
	if err != nil {
		fmt.Println("Error receiving converted audio stream ", err)
		go s.ChannelMessageSend(channelID, "Error while receiving converted audio stream")
		return err
	}
	defer ffmpegout.Close()
	ffmpegbuf := bufio.NewReaderSize(ffmpegout, 16384)
	err = run.Start()
	if err != nil {
		fmt.Println("Error starting FFmpeg: ", err)
		go s.ChannelMessageSend(channelID, "Error while starting stream transformer")
		panic(err)
	}
	defer run.Process.Kill()
	g.vc.Speaking(true)
	defer g.vc.Speaking(false)
	send := make(chan []int16, 960*2)
	wg := sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	complete := make(chan struct{})
	go func() {
		SendPCM(ctx, g.vc, send, &wg)
		complete <- struct{}{}
	}()
	go s.ChannelMessageSend(channelID, fmt.Sprintf("Now playing: %s", video.Title))
	defer func() {
		if err := recover(); err != nil {
			if erro, ok := err.(error); ok && erro.Error() == "send on closed channel" {
				return
			}
			fmt.Println("Error playing sound: ", err)
			go s.ChannelMessageSend(channelID, "Error playing sound")
		}
		g.queue = g.queue[1:]
	}()
	vcUpdateListener := g.vcUpdateBroker.GetVoiceUpdateListener()
	defer g.vcUpdateBroker.ClearVoiceUpdateListener(vcUpdateListener)
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
		wg.Wait()
		select {
		case update := <-vcUpdateListener:
			if update == "No members in voice channel" {
				wg.Add(1)
				go func() {
					defer wg.Done()
					select {
					case update = <-vcUpdateListener:
						if update == "User joined voice channel" {
							go s.ChannelMessageSend(channelID, "Resuming audio.")
							return
						}
					case <-time.After(1 * time.Minute):
						go s.ChannelMessageSend(channelID, "No members in voice channel. Stopping audio.")
						cancel()
						return
					}
				}()
			}
			if update == "User requested stop" {
				cancel()
				return
			}
		default:
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-timer.C:
				cancel()
				return nil
			case send <- audiobuf:
				timer.Reset(50 * time.Millisecond)
			}
		}
	}
	<-complete
	cancel()
	return nil
}

func SendPCM(ctx context.Context, v *discordgo.VoiceConnection, pcm <-chan []int16, wg *sync.WaitGroup) {
	if pcm == nil {
		return
	}
	opusEncoder, err := gopus.NewEncoder(48000, 2, gopus.Audio)
	if err != nil {
		fmt.Println("Error creating opus encoder: ", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
			recv, ok := <-pcm
			if !ok || recv == nil {
				return
			}
			wg.Wait()
			opus, err := opusEncoder.Encode(recv, 960, 960*2)
			if err != nil {
				fmt.Println("Error encoding PCM: ", err)
				return
			}
			if !v.Ready || v.OpusSend == nil {
				return
			}
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-timer.C:
				return
			case v.OpusSend <- opus:
				timer.Reset(50 * time.Millisecond)
			}
		}
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
	result, err := http.Get("https://www.youtube.com/results?search_query=" + url.QueryEscape(query+" audio"))
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
