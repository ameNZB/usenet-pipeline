package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
	"github.com/ameNZB/usenet-pipeline/utils"

	"github.com/google/uuid"
)

const ChunkSize = 700 * 1024 // 700KB — Nyuu/Usenet standard article size

// bufPool reuses chunk read buffers to avoid per-chunk allocation.
var bufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, ChunkSize)
		return &b
	},
}

// yencPool reuses yEnc output buffers. Encoded output is roughly 1-2% larger
// than input due to escaping, plus ~200 bytes of headers/trailer.
var yencPool = sync.Pool{
	New: func() interface{} {
		b := bytes.NewBuffer(make([]byte, 0, ChunkSize+ChunkSize/50+256))
		return b
	},
}

type UploadJob struct {
	ChunkData   []byte
	Number      int
	TotalParts  int
	Subject     string
	FileName    string
	ChunkOffset int64
	TotalSize   int64
}

type NZBSegment struct {
	Bytes     int    `xml:"bytes,attr"`
	Number    int    `xml:"number,attr"`
	MessageID string `xml:",chardata"`
}

// UploadDirectory uploads all files in a directory to Usenet and returns
// per-file segment lists suitable for NZB generation. Files are uploaded
// with obfuscated subjects; the real names are only in the NZB.
func UploadDirectory(ctx context.Context, cfg *config.Config, dir string, jobName string) ([]FileSegments, error) {
	var files []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() == 0 {
			return err
		}
		files = append(files, path)
		return nil
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to upload in %s", dir)
	}

	// Calculate total size across all files for cumulative progress.
	var totalDirSize int64
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			totalDirSize += info.Size()
		}
	}
	var cumulativeUploaded int64

	var allFiles []FileSegments
	for i, filePath := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Use relative path so NZB preserves subdirectory structure.
		relName, _ := filepath.Rel(dir, filePath)
		relName = filepath.ToSlash(relName) // normalize to forward slashes for NZB
		// Obfuscated subject prevents filename leaking on Usenet.
		obfSubject := GenerateRandomPassword(16)

		storage.UpdateState(jobName, "Uploading",
			fmt.Sprintf("File %d/%d: %s", i+1, len(files), relName), 0)

		segments, err := UploadToUsenet(ctx, cfg, filePath, obfSubject, jobName, cumulativeUploaded, totalDirSize)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", relName, err)
		}

		// Track cumulative bytes for next file's progress offset.
		if info, err := os.Stat(filePath); err == nil {
			cumulativeUploaded += info.Size()
		}

		// Build the standard subject for the NZB entry.
		totalParts := len(segments)
		stat, _ := os.Stat(filePath)
		nzbSubject := fmt.Sprintf("[%d/%d] - \"%s\" yEnc (1/%d) %d",
			i+1, len(files), relName, totalParts, stat.Size())

		allFiles = append(allFiles, FileSegments{
			FileName: nzbSubject,
			Segments: segments,
		})
	}
	return allFiles, nil
}

// UploadToUsenet chunks the file and uploads it using a worker pool.
func UploadToUsenet(ctx context.Context, cfg *config.Config, filePath string, subject string, jobName string, cumulativeBytes int64, totalDirSize int64) ([]NZBSegment, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	stat, _ := file.Stat()
	totalChunks := int((stat.Size() + ChunkSize - 1) / ChunkSize)

	// Fixed-size channel buffers prevent unbounded memory growth on large files.
	workerCount := cfg.NNTPConnections
	jobs := make(chan UploadJob, workerCount*2)
	results := make(chan NZBSegment, workerCount*2)
	errs := make(chan error, 1) // first fatal error

	var wg sync.WaitGroup

	// Start workers.
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go nntpWorker(ctx, cfg, jobs, results, errs, &wg)
	}

	// Dispatch jobs (read file in chunks).
	go func() {
		defer close(jobs)
		fileName := filepath.Base(filePath)
		for i := 1; i <= totalChunks; i++ {
			if ctx.Err() != nil {
				return
			}

			// Get a buffer from the pool.
			bp := bufPool.Get().(*[]byte)
			buffer := *bp
			n, err := file.Read(buffer)
			if err != nil && err != io.EOF {
				select {
				case errs <- fmt.Errorf("read chunk %d: %v", i, err):
				default:
				}
				bufPool.Put(bp)
				return
			}
			if n == 0 {
				bufPool.Put(bp)
				break
			}

			// Copy the data out so we can return the buffer to the pool.
			// The copy is needed because the worker will hold ChunkData
			// until upload completes, while we reuse the read buffer.
			chunkData := make([]byte, n)
			copy(chunkData, buffer[:n])
			bufPool.Put(bp)

			offset := int64(i-1) * ChunkSize
			jobs <- UploadJob{
				ChunkData:   chunkData,
				Number:      i,
				TotalParts:  totalChunks,
				Subject:     fmt.Sprintf("%s [%d/%d]", subject, i, totalChunks),
				FileName:    fileName,
				ChunkOffset: offset,
				TotalSize:   stat.Size(),
			}
		}
	}()

	// Close results when all workers finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect segments.
	var segments []NZBSegment
	uploadedCount := 0
	var uploadedBytes int64
	startTime := time.Now()

	for seg := range results {
		if ctx.Err() != nil {
			continue // drain channel
		}

		segments = append(segments, seg)
		uploadedCount++
		uploadedBytes += int64(seg.Bytes)

		// Per-file percent for state string.
		filePercent := float64(uploadedCount) / float64(totalChunks) * 100

		// Cumulative percent across all files in the directory.
		var overallPercent float64
		if totalDirSize > 0 {
			overallPercent = float64(cumulativeBytes+uploadedBytes) / float64(totalDirSize) * 100
		} else {
			overallPercent = filePercent
		}

		elapsed := time.Since(startTime).Seconds()
		speed := 0.0
		if elapsed > 0 {
			speed = float64(uploadedBytes) / elapsed / 1024 / 1024
		}

		etaStr := "Calculating..."
		if speed > 0 {
			var remainingMB float64
			if totalDirSize > 0 {
				remainingMB = float64(totalDirSize-cumulativeBytes-uploadedBytes) / 1024 / 1024
			} else {
				remainingMB = (float64(totalChunks*ChunkSize) - float64(uploadedBytes)) / 1024 / 1024
			}
			etaSeconds := remainingMB / speed
			etaStr = utils.FormatETA(etaSeconds)
		}

		storage.UpdateState(jobName, "Uploading", fmt.Sprintf("%.1f%% - %.2f MB/s - ETA: %s", overallPercent, speed, etaStr), overallPercent)

		if cb := GetProgressCallback(jobName); cb != nil {
			cb(speed, overallPercent, "uploading", 0)
		}
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Check for worker errors.
	select {
	case err := <-errs:
		return nil, err
	default:
	}

	return segments, nil
}

func nntpWorker(ctx context.Context, cfg *config.Config, jobs <-chan UploadJob, results chan<- NZBSegment, errs chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	var conn *textproto.Conn

	for job := range jobs {
		if ctx.Err() != nil {
			return
		}

		// Generate a unique Message-ID.
		domain := cfg.NNTPDomain
		if domain == "" {
			if parts := strings.Split(cfg.NNTPFrom, "@"); len(parts) == 2 {
				domain = parts[1]
			} else {
				domain = "example.com"
			}
		}
		messageID := fmt.Sprintf("%s@%s", uuid.New().String(), domain)

		// yEnc encode the chunk using a pooled buffer.
		encodedData := yEncodeChunk(job.ChunkData, job.FileName, job.Number, job.TotalParts, job.ChunkOffset, job.TotalSize)

		maxRetries := 3
		success := false

		for attempt := 1; attempt <= maxRetries; attempt++ {
			if conn == nil {
				var err error
				conn, err = connectNNTP(cfg)
				if err != nil {
					log.Printf("Worker connection failed (attempt %d): %v", attempt, err)
					backoff(attempt)
					continue
				}
			}

			err := uploadChunk(cfg, conn, job, messageID, encodedData)
			if err != nil {
				log.Printf("Chunk %d upload failed (attempt %d): %v", job.Number, attempt, err)
				conn.Close()
				conn = nil
				backoff(attempt)
				continue
			}

			success = true
			break
		}

		if !success {
			err := fmt.Errorf("chunk %d failed after %d attempts", job.Number, maxRetries)
			log.Printf("FATAL: %v", err)
			select {
			case errs <- err:
			default:
			}
			return // exit worker, don't abort the entire process
		}

		log.Printf("Uploaded chunk %d - MsgID: %s", job.Number, messageID)

		results <- NZBSegment{
			Bytes:     len(job.ChunkData),
			Number:    job.Number,
			MessageID: messageID,
		}
	}

	if conn != nil {
		conn.PrintfLine("QUIT")
		conn.Close()
	}
}

// backoff sleeps with exponential backoff: 2s, 4s, 8s.
func backoff(attempt int) {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	time.Sleep(d)
}

func connectNNTP(cfg *config.Config) (*textproto.Conn, error) {
	var netConn net.Conn
	var err error

	if cfg.NNTPSSL {
		netConn, err = tls.Dial("tcp", cfg.NNTPServer, nil)
	} else {
		netConn, err = net.Dial("tcp", cfg.NNTPServer)
	}

	if err != nil {
		return nil, err
	}
	conn := textproto.NewConn(netConn)

	if _, _, err = conn.ReadCodeLine(0); err != nil {
		conn.Close()
		return nil, err
	}

	if cfg.NNTPUser != "" {
		if err = conn.PrintfLine("AUTHINFO USER %s", cfg.NNTPUser); err != nil {
			conn.Close()
			return nil, err
		}
		if _, _, err = conn.ReadCodeLine(381); err == nil {
			if err = conn.PrintfLine("AUTHINFO PASS %s", cfg.NNTPPass); err != nil {
				conn.Close()
				return nil, err
			}
			if _, _, err = conn.ReadCodeLine(281); err != nil {
				conn.Close()
				return nil, fmt.Errorf("NNTP Auth failed: %v", err)
			}
		}
	}
	return conn, nil
}

func uploadChunk(cfg *config.Config, conn *textproto.Conn, job UploadJob, messageID string, encodedData []byte) error {
	if err := conn.PrintfLine("POST"); err != nil {
		return err
	}
	if _, _, err := conn.ReadCodeLine(340); err != nil {
		return err
	}

	dw := conn.DotWriter()
	fmt.Fprintf(dw, "From: %s\r\n", cfg.NNTPFrom)
	fmt.Fprintf(dw, "Newsgroups: %s\r\n", cfg.NNTPGroup)
	fmt.Fprintf(dw, "Subject: %s\r\n", job.Subject)
	fmt.Fprintf(dw, "Message-ID: <%s>\r\n", messageID)
	fmt.Fprintf(dw, "\r\n")

	if _, err := dw.Write(encodedData); err != nil {
		dw.Close()
		return err
	}
	if err := dw.Close(); err != nil {
		return err
	}

	_, _, err := conn.ReadCodeLine(240)
	return err
}

// yEncodeChunk encodes a buffer into yEnc format for a specific part.
// Uses a pooled buffer to avoid per-chunk allocation.
func yEncodeChunk(data []byte, filename string, partNumber int, totalParts int, chunkOffset int64, totalSize int64) []byte {
	maxCap := ChunkSize*2 + 256
	buf := yencPool.Get().(*bytes.Buffer)
	// Prevent unbounded capacity growth: discard oversized buffers.
	if buf.Cap() > maxCap {
		buf = bytes.NewBuffer(make([]byte, 0, ChunkSize+ChunkSize/50+256))
	}
	buf.Reset()

	crc := crc32.ChecksumIEEE(data)

	// Headers.
	fmt.Fprintf(buf, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n", partNumber, totalParts, totalSize, filename)
	fmt.Fprintf(buf, "=ypart begin=%d end=%d\r\n", chunkOffset+1, chunkOffset+int64(len(data)))

	// Encode data.
	lineLen := 0
	for _, b := range data {
		val := (b + 42) & 255

		if val == 0 || val == 10 || val == 13 || val == 61 || (lineLen == 0 && val == 46) {
			buf.WriteByte('=')
			buf.WriteByte((val + 64) & 255)
			lineLen += 2
		} else {
			buf.WriteByte(val)
			lineLen++
		}

		if lineLen >= 128 {
			buf.WriteString("\r\n")
			lineLen = 0
		}
	}
	if lineLen > 0 {
		buf.WriteString("\r\n")
	}

	// Trailer.
	fmt.Fprintf(buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(data), partNumber, crc)

	// Copy out so we can return the buffer to the pool.
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	yencPool.Put(buf)
	return result
}
