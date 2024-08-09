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
	"sort"
	"strings"
	"sync"
	"syscall"

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

type GuildPlayer struct {
	vc       *discordgo.VoiceConnection
	queue    []string
	ytClient youtube.Client
	stopChan chan struct{}
	members  []string
	mx       sync.RWMutex
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
	dg.AddHandler(func(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		g, ok := MusicPlayerInstance.GuildPlayers[vs.GuildID]
		if !ok {
			return
		}
		if vs.BeforeUpdate == nil || vs.Member.User.Bot {
			return
		}
		g.mx.RLock()
		// if the user is not in the voice channel before the update and is in the voice channel after the update
		if vs.BeforeUpdate.ChannelID != g.vc.ChannelID && vs.ChannelID == g.vc.ChannelID {

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
			}
		}
		g.mx.RUnlock()
		// if the user is in the voice channel before the update and is not in the voice channel after the update
		if vs.BeforeUpdate.ChannelID == g.vc.ChannelID && vs.ChannelID != g.vc.ChannelID {
			g.mx.Lock()
			for i, member := range g.members {
				if member == vs.UserID {
					g.members = append(g.members[:i], g.members[i+1:]...)
					break
				}
			}
			g.mx.Unlock()
		}
		if len(g.members) <= 1 {
			g.stopChan <- struct{}{}
		}
	})
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
					defer guild.mx.Unlock()
					guild.queue = append(guild.queue, id)
					return
				}
				MusicPlayerInstance.GuildPlayers[g.ID] = &GuildPlayer{
					queue:    []string{id},
					ytClient: youtube.Client{},
					stopChan: make(chan struct{}),
					mx:       sync.RWMutex{},
				}
				go MusicPlayerInstance.GuildPlayers[g.ID].StartPlayer(s, g.ID, vs.ChannelID)
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
			guild.stopChan <- struct{}{}
			return
		}
		s.ChannelMessageSendReply(m.ChannelID, "No audio is currently playing in this guild.", m.Reference())
	}
}

func (g *GuildPlayer) StartPlayer(s *discordgo.Session, guildID, channelID string) (err error) {
	g.mx.Lock()
	g.vc, err = s.ChannelVoiceJoin(guildID, channelID, false, true)
	defer func() {
		g.mx.Lock()
		g.vc.Disconnect()
		g.vc.Close()
		close(g.stopChan)
		delete(MusicPlayerInstance.GuildPlayers, guildID)
		g.mx.Unlock()
	}()
	if err != nil {
		g.mx.Unlock()
		return err
	}
	ch, err := s.Channel(g.vc.ChannelID)
	if err != nil {
		fmt.Println("Error getting channel: ", err)
		g.mx.Unlock()
		return
	}
	g.members = []string{}
	for _, member := range ch.Members {
		g.members = append(g.members, member.UserID)
	}
	g.mx.Unlock()
	for len(g.queue) > 0 {
		err = g.playSound(s, channelID)
		if err != nil {
			fmt.Println("Error playing sound: in StartPlayer", err)
			return err
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
		return f.MimeType == "audio/webm; codecs=\"opus\""
	})
	formats.Sort()
	run := exec.Command("ffmpeg", "-i", "pipe:0", "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
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
	send := make(chan []int16, 960*2)
	defer close(send)
	go SendPCM(g.vc, send)
	s.ChannelMessageSend(channelID, fmt.Sprintf("Now playing: %s", video.Title))
	defer func() {
		g.queue = g.queue[1:]
	}()
	for {
		audiobuf := make([]int16, 960*2)
		err = binary.Read(ffmpegbuf, binary.LittleEndian, &audiobuf)
		if err != nil {
			if err.Error() == "EOF" || err.Error() == "unexpected EOF" {
				send <- nil
				err = nil
				return
			}
			send <- nil
			return
		}
		select {
		case <-g.stopChan:
			send <- nil
			return
		case send <- audiobuf:
		}
	}
}

func SendPCM(v *discordgo.VoiceConnection, pcm <-chan []int16) {
	if pcm == nil {
		return
	}
	opusEncoder, err := gopus.NewEncoder(48000, 2, gopus.Audio)
	if err != nil {
		fmt.Println("Error creating opus encoder: ", err)
		return
	}
	for {
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
	result, err := http.Get("https://www.youtube.com/results?search_query=" + url.QueryEscape(query+" audio"))
	if err != nil {
		return "", err
	}
	resBytes, err := io.ReadAll(result.Body)
	if err != nil {
		return "", err
	}
	regexp := regexp.MustCompile(`(?:<script.*?>.*?\n.*?)(?:\[{"v.*?:")(.{11})`)
	res := regexp.FindAllSubmatch(resBytes, 1)
	if len(res) > 0 {
		return string(res[0][1]), nil
	}
	return "", nil
}
