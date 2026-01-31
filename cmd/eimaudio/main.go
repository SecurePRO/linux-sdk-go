// Command eimaudio launches an audio (microphone) model process, records audio
// from your microphone, and classifies the audio samples using the model.
//
// Examples:
//
//	# Classify audio samples with default settings.
//	eimaudio ../../custom-keywords.eim
//
//	# Classify audio, and apply a moving average filter with a history of 4.
//	eimaudio -maf 4 -verbose -interval 250ms ../../custom-keywords.eim
//
//	# List audio devices, to be used with the -device flag.
//	eimaudio -listdevices
//
//	# List audio devices, to be used with the -device flag.
//	eimaudio -device hw:0,0 ../../custom-keywords.eim
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"encoding/binary"
	"path/filepath"

	edgeimpulse "github.com/edgeimpulse/linux-sdk-go"
	"github.com/edgeimpulse/linux-sdk-go/audio"
	"github.com/edgeimpulse/linux-sdk-go/audio/audiocmd"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// recorderWrapper wraps a recorder to allow audio stream duplication
type recorderWrapper struct {
	reader io.Reader
	closer io.Closer
}

func (rw *recorderWrapper) Reader() io.Reader {
	return rw.reader
}

func (rw *recorderWrapper) Close() error {
	return rw.closer.Close()
}

var (
	listDevices          bool
	interval             time.Duration
	mafSize              int
	verbose              bool
	traceDir             string
	deviceID             string
	obsMicrophoneID      string
	mqTopic              string
	obsDeviceID          string
	recordStopDuration   time.Duration
	recordOnTrigger      bool
	recordDir            string
)

func init() {
	flag.BoolVar(&listDevices, "listdevices", false, "if set, lists devices and exits")
	flag.DurationVar(&interval, "interval", 250*time.Millisecond, "classify audio every interval")
	flag.IntVar(&mafSize, "maf", 0, "apply moving-average-filter for all labels of the model of given size (only if >0)")
	flag.BoolVar(&verbose, "verbose", false, "print more logging")
	flag.StringVar(&traceDir, "tracedir", "", "if set, store the parsed classify data to the named directory")
	flag.StringVar(&deviceID, "device", "", "if set, device ID is used for microphone instead of the default microphone")
	flag.StringVar(&mqTopic, "topic", "classification", "if set, this is the MQTT Topic that the event will publish to (default: classification)")
	flag.StringVar(&obsDeviceID, "obsDeviceID", "", "Sets the device ID in the mqtt topic /device_xxxx/")
	flag.DurationVar(&recordStopDuration, "recordstop", 10*time.Second, "Duration of continuous background before stopping recording.")
	flag.BoolVar(&recordOnTrigger, "record", false, "If set, records audio when trigger label is detected.")
	flag.StringVar(&recordDir, "recorddir", "/etc/obs_engine/video/audio_clips/", "directory to save audio recordings")
	flag.StringVar(&obsMicrophoneID, "obsMicrophoneID", "", "Sets the microphone ID for the recorded wav filename.")
}

func usage() {
	log.Println("usage: eimaudio [flags] model")
	flag.PrintDefaults()
	os.Exit(2)
}

// Add this function to save WAV files
func saveWAVFile(filename string, samples []int16, sampleRate int) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// WAV header
	dataSize := len(samples) * 2
	// RIFF header
	f.Write([]byte("RIFF"))
	binary.Write(f, binary.LittleEndian, uint32(36+dataSize))
	f.Write([]byte("WAVE"))
	
	// fmt chunk
	f.Write([]byte("fmt "))
	binary.Write(f, binary.LittleEndian, uint32(16))           // chunk size
	binary.Write(f, binary.LittleEndian, uint16(1))            // audio format (PCM)
	binary.Write(f, binary.LittleEndian, uint16(1))            // num channels
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))   // sample rate
	binary.Write(f, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(f, binary.LittleEndian, uint16(2))            // block align
	binary.Write(f, binary.LittleEndian, uint16(16))           // bits per sample
	
	// data chunk
	f.Write([]byte("data"))
	binary.Write(f, binary.LittleEndian, uint32(dataSize))
	binary.Write(f, binary.LittleEndian, samples)
	
	return nil
}

func main() {
	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	os.Exit(main0(args))
}

func main0(args []string) int {
	if listDevices {
		devs, err := audiocmd.ListDevices()
		if err != nil {
			log.Fatalf("listing devices: %v", err)
		}
		for _, dev := range devs {
			log.Printf("%v: %v", dev.ID, dev.Name)
		}
		os.Exit(0)
	}

	if len(args) != 1 {
		usage()
	}

	if obsDeviceID == "" {
		fmt.Println("Error: -obsDeviceID is required")
		flag.Usage()
		os.Exit(1)
	}

	if obsMicrophoneID == "" {
		fmt.Println("Error: -obsMicrophoneID is required")
		flag.Usage()
		os.Exit(1)
	}

	if recordOnTrigger {
		if err := os.MkdirAll(recordDir, 0755); err != nil {
			log.Printf("creating record directory: %v", err)
			return 1
		}
	}

	// Setup MQTT Client
	opts := mqtt.NewClientOptions()
	opts.AddBroker("tcp://localhost:1883")
	opts.SetUsername("observables")
	opts.SetPassword("mqTT117obs")
	opts.SetClientID("audio_classifier_" + mqTopic)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetAutoReconnect(true)

	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Printf("MQTT connection error: %v", token.Error())
		return 1
	}
	defer mqttClient.Disconnect(250)
	log.Printf("Connected to MQTT broker")

	ropts := &edgeimpulse.RunnerOpts{
		TraceDir: traceDir,
	}
	runner, err := edgeimpulse.NewRunnerProcess(args[0], ropts)
	if err != nil {
		log.Printf("new runner: %v", err)
		return 1
	}
	defer runner.Close()

	log.Printf("project %s\nmodel %s", runner.Project(), runner.ModelParameters())

	recOpts := &audiocmd.RecorderOpts{
		SampleRate:    int(runner.ModelParameters().Frequency),
		Channels:      1,
		AsRaw:         true,
		RecordProgram: "sox",
		Verbose:       verbose,
		DeviceID:      deviceID,
	}
	recorder, err := audiocmd.NewRecorder(recOpts)
	if err != nil {
		log.Printf("new recorder: %v", err)
		return 1
	}
	defer recorder.Close()

	/////////
	//Wrapped Recoder Concept

	// Buffer to store recent audio samples (circular buffer)
	bufferDuration := 3 * time.Second
	bufferSize := int(float64(recOpts.SampleRate) * bufferDuration.Seconds())
	audioBuffer := make([]int16, bufferSize)
	bufferPos := 0

	// Channel for audio samples
	audioChan := make(chan []int16, 100)

	// Create a pipe to duplicate the audio stream
	pipeReader, pipeWriter := io.Pipe()

	// Wrap the recorder's reader to write to both the classifier and our capture
	go func() {
		reader := recorder.Reader()
		buf := make([]byte, 4096)
	
		for {
			n, err := reader.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading audio: %v", err)
				}
				pipeWriter.Close()
				close(audioChan)
				return
			}
		
			// Write to pipe for classifier
			if _, err := pipeWriter.Write(buf[:n]); err != nil {
				log.Printf("Error writing to pipe: %v", err)
				close(audioChan)
				return
			}
			
			// Convert bytes to int16 samples for our capture
			numSamples := n / 2
			samples := make([]int16, numSamples)
			for i := 0; i < numSamples; i++ {
				samples[i] = int16(buf[i*2]) | int16(buf[i*2+1])<<8
			}
		
			audioChan <- samples
		}
	}()

	wrappedRecorder := &recorderWrapper{
		reader: pipeReader,
		closer: recorder,
	}
	////////

	copts := &audio.ClassifierOpts{
		Verbose: verbose,
	}
	ac, err := audio.NewClassifier(runner, wrappedRecorder, interval, copts)
	if err != nil {
		log.Printf("new audio classifier: %v", err)
		return 1
	}
	defer ac.Close()

	var maf *edgeimpulse.MAF
	if mafSize > 0 {
		if verbose {
			log.Printf("applying moving average filter of size %d", mafSize)
		}
		maf, err = edgeimpulse.NewMAF(mafSize, runner.ModelParameters().Labels)
		if err != nil {
			log.Printf("new MAF: %v", err)
		}
	}

	// Handle signals, so cleanup of the runners temporary directory is done.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	// Keep reading classification events.
	var previousLabel string = "" // Keep track of the previous label to supress 'background' events
	var recording bool = false
	var lastNonBackgroundTime time.Time 
	var recordingBuffer []int16
	
	for {
		select {
		case <-signals:
			return 1

		// Handle incoming audio samples from the recorder
		case samples, ok := <-audioChan:
			if !ok {
				log.Printf("Audio stream closed")
				return 0
			}
		
			// Store in circular buffer
			for _, sample := range samples {
				audioBuffer[bufferPos] = sample
				bufferPos = (bufferPos + 1) % bufferSize
			
				// If recording, also add to recording buffer
				if recording {
					recordingBuffer = append(recordingBuffer, sample)
				}
			}

		// Handle classification events
		case ev, ok := <-ac.Events:
			if !ok {
				log.Printf("no more events")
				return 0
			}
			if ev.Err != nil {
				log.Printf("%s", ev.Err)
			} else {
				if maf != nil {
					r, err := maf.Update(ev.RunnerClassifyResponse.Result.Classification)
					if err != nil {
						log.Printf("update moving average filter: %v", err)
					}
					ev.RunnerClassifyResponse.Result.Classification = r
				}
				fmt.Printf("%s\n", ev.RunnerClassifyResponse)
				fmt.Printf("%v\n", ev.RunnerClassifyResponse.Result.Classification)

				// Find label with the Highest Confidence
				var maxLabel string
				var maxConfidence float64
				for label, confidence := range ev.RunnerClassifyResponse.Result.Classification {
					if confidence > maxConfidence {
						maxConfidence = confidence
						maxLabel = label
					}
				}

				// Start recording when a non-background label is detected
				if !recording && maxLabel != "background" {
					recording = true
					lastNonBackgroundTime = time.Now()
					recordingBuffer = make([]int16, 0)

					// Pre-fill with content from circular buffer (captures audio before trigger)
					orderedSamples := make([]int16, bufferSize)
					copy(orderedSamples, audioBuffer[bufferPos:])
					copy(orderedSamples[bufferSize-bufferPos:], audioBuffer[:bufferPos])
					recordingBuffer = append(recordingBuffer, orderedSamples...)

					if verbose {
						log.Printf("Started recording (triggered by: %s)", maxLabel)
					}
				}

				// during recording, track consecutive background detections
				if recording {
					if verbose {
						log.Printf("-> RECORDING IS IN PROGRESS")
					}
					if maxLabel == "background" {
						if time.Since(lastNonBackgroundTime) >= recordStopDuration {
							recording = false

							// Stop recording and Save the File
							timestamp := time.Now().Format("20060102150405")
							filename := filepath.Join(recordDir, fmt.Sprintf("%s_%s.wav", obsMicrophoneID, timestamp))

							if err := saveWAVFile(filename, recordingBuffer, recOpts.SampleRate); err != nil {
								log.Printf("Error saving audio: %v", err)
							} else {
								log.Printf("Saved audio recording: %s (%.1f seconds, %d samples)", 
									filename, 
									float64(len(recordingBuffer))/float64(recOpts.SampleRate),
									len(recordingBuffer))
							}
						
							// Clear recording buffer
							recordingBuffer = nil

							if verbose {
								log.Printf("STOPPED RECORDING (%.1f seconds of background)", time.Since(lastNonBackgroundTime).Seconds())
							}
						}
					} else {
						// Reset timer if a non-background label is detected during recording
						lastNonBackgroundTime = time.Now()
						if verbose {
							log.Printf("-> RECORDING TIMER HAS BEEN RESET")
						}
					}
				}


				// Only publish to MQTT if:
				// 1. Label is NOT "background", OR
				// 2. Label is "background", but previous label was NOT "background"
				if maxLabel != "background" || previousLabel != "background" {
					topic := "observables/device_"+ obsDeviceID + "/audio/" + mqTopic
					payload := fmt.Sprintf(`{"label":"%s","confidence":%.2f}`, maxLabel, maxConfidence)
					token := mqttClient.Publish(topic, 0, false, payload)
					token.Wait()
					if token.Error() != nil {
						log.Printf("MQTT publish error: %v", token.Error())
					} else if verbose {
						log.Printf("Published to MQTT: %s", payload)
					}
				} else {
					if verbose {
						log.Printf("Suppressed background notification (already in background state)")
					}
				}

				// Update the previous label for next iteration
				previousLabel = maxLabel
			}
		}
	}
}
