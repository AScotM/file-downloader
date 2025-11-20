package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Downloader struct {
	DownloadDir    string
	Timeout        time.Duration
	ConnectTimeout time.Duration
	UserAgent      string
	MaxRedirects   int
	MaxRetries     int
	BufferSize     int
	MaxSpeed       int64
	MaxSize        int64
	VerifySSL      bool
}

type DownloadResult struct {
	Success      bool
	Filename     string
	Filepath     string
	Size         int64
	DownloadTime time.Duration
	AverageSpeed float64
	Resumed      bool
	Error        string
	URL          string
	Retries      int
}

func NewDownloader(downloadDir string) *Downloader {
	if downloadDir == "" {
		downloadDir = "."
	}

	return &Downloader{
		DownloadDir:    downloadDir,
		Timeout:        0,
		ConnectTimeout: 30 * time.Second,
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
		MaxRedirects:   10,
		MaxRetries:     3,
		BufferSize:     32 * 1024,
		MaxSpeed:       0,
		MaxSize:        0,
		VerifySSL:      true,
	}
}

func (d *Downloader) SetTimeout(timeout time.Duration) *Downloader {
	d.Timeout = timeout
	return d
}

func (d *Downloader) SetConnectTimeout(timeout time.Duration) *Downloader {
	d.ConnectTimeout = timeout
	return d
}

func (d *Downloader) SetMaxRedirects(redirects int) *Downloader {
	d.MaxRedirects = redirects
	return d
}

func (d *Downloader) SetMaxRetries(retries int) *Downloader {
	d.MaxRetries = retries
	return d
}

func (d *Downloader) SetBufferSize(size int) *Downloader {
	d.BufferSize = size
	return d
}

func (d *Downloader) SetMaxSpeed(speed int64) *Downloader {
	d.MaxSpeed = speed
	return d
}

func (d *Downloader) SetMaxSize(size int64) *Downloader {
	d.MaxSize = size
	return d
}

func (d *Downloader) SetVerifySSL(verify bool) *Downloader {
	d.VerifySSL = verify
	return d
}

func (d *Downloader) Download(urlStr, customFilename string) *DownloadResult {
	startTime := time.Now()
	result := &DownloadResult{URL: urlStr}

	if err := d.validateURL(urlStr); err != nil {
		result.Error = err.Error()
		return result
	}

	if err := d.checkDiskSpace(); err != nil {
		result.Error = err.Error()
		return result
	}

	if err := os.MkdirAll(d.DownloadDir, 0755); err != nil {
		result.Error = fmt.Sprintf("Cannot create download directory: %v", err)
		return result
	}

	filename := customFilename
	if filename == "" {
		filename = d.generateFilename(urlStr)
	}

	safeFilename, err := d.sanitizeFilename(filename)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	filepath := filepath.Join(d.DownloadDir, safeFilename)

	var lastError error
	for retry := 0; retry <= d.MaxRetries; retry++ {
		result.Retries = retry
		resumeFrom := int64(0)
		if d.canResume(filepath) {
			if info, err := os.Stat(filepath); err == nil {
				resumeFrom = info.Size()
				result.Resumed = true
			}
		}

		size, err := d.downloadFile(urlStr, filepath, resumeFrom)
		if err == nil {
			result.Success = true
			result.Filename = safeFilename
			result.Filepath = filepath
			result.Size = size
			result.DownloadTime = time.Since(startTime)
			if result.DownloadTime > 0 {
				result.AverageSpeed = float64(size) / result.DownloadTime.Seconds()
			}
			return result
		}

		lastError = err
		if retry < d.MaxRetries {
			backoff := time.Duration(retry+1) * 2 * time.Second
			fmt.Printf("Download failed, retrying in %v... (attempt %d/%d)\n", backoff, retry+1, d.MaxRetries)
			time.Sleep(backoff)
		}
	}

	result.Error = fmt.Sprintf("All retries failed: %v", lastError)
	if fileInfo, err := os.Stat(filepath); err == nil && fileInfo.Size() == 0 {
		os.Remove(filepath)
	}
	return result
}

func (d *Downloader) validateURL(urlStr string) error {
	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only HTTP and HTTPS URLs are supported")
	}
	if u.Host == "" {
		return errors.New("URL must contain a host")
	}
	return nil
}

func (d *Downloader) sanitizeFilename(filename string) (string, error) {
	if filename == "" {
		return "", errors.New("filename cannot be empty")
	}

	clean := filepath.Clean(filename)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("invalid filename")
	}

	return clean, nil
}

func (d *Downloader) checkDiskSpace() error {
	var stat syscall.Statfs_t
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}

	err = syscall.Statfs(wd, &stat)
	if err != nil {
		return nil
	}

	freeSpace := stat.Bavail * uint64(stat.Bsize)
	if freeSpace < 100*1024*1024 {
		return fmt.Errorf("insufficient disk space: %s available", formatBytes(int64(freeSpace)))
	}

	return nil
}

func (d *Downloader) generateFilename(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Sprintf("download_%d", time.Now().Unix())
	}

	path := u.Path
	if path == "" || path == "/" {
		return fmt.Sprintf("download_%d", time.Now().Unix())
	}

	filename := filepath.Base(path)
	if filename == "" || filename == "." || filename == "/" {
		return fmt.Sprintf("download_%d", time.Now().Unix())
	}

	return filename
}

func (d *Downloader) canResume(filepath string) bool {
	info, err := os.Stat(filepath)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

func (d *Downloader) createHTTPClient() *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !d.VerifySSL,
		},
		DialContext: (&net.Dialer{
			Timeout:   d.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   d.Timeout,
	}

	if d.MaxRedirects > 0 {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= d.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", d.MaxRedirects)
			}
			return nil
		}
	}

	return client
}

func (d *Downloader) downloadFile(urlStr, filepath string, resumeFrom int64) (int64, error) {
	ctx := context.Background()
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), d.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("User-Agent", d.UserAgent)
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	client := d.createHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("HTTP error: %s", resp.Status)
	}

	if d.MaxSize > 0 {
		contentLength := resp.ContentLength
		if resp.Header.Get("Content-Range") != "" {
			contentLength += resumeFrom
		}
		if contentLength > d.MaxSize {
			return 0, fmt.Errorf("file size %s exceeds maximum allowed %s",
				formatBytes(contentLength), formatBytes(d.MaxSize))
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumeFrom > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	file, err := os.OpenFile(filepath, flags, 0644)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var totalSize int64
	if resp.Header.Get("Content-Range") != "" {
		totalSize = resumeFrom + resp.ContentLength
	} else if resp.ContentLength > 0 {
		totalSize = resp.ContentLength
	} else {
		totalSize = -1
	}

	if d.MaxSize > 0 && totalSize > d.MaxSize {
		return 0, fmt.Errorf("file size %s exceeds maximum allowed %s",
			formatBytes(totalSize), formatBytes(d.MaxSize))
	}

	reader := bufio.NewReader(resp.Body)
	buffer := make([]byte, d.BufferSize)
	var totalWritten int64 = resumeFrom
	lastUpdate := time.Now()
	startTime := time.Now()
	lastBytes := resumeFrom

	fmt.Printf("Downloading: %s\n", urlStr)
	if resumeFrom > 0 {
		fmt.Printf("Resuming from: %s\n", formatBytes(resumeFrom))
	}

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if d.MaxSpeed > 0 {
				expectedTime := time.Duration(n) * time.Second / time.Duration(d.MaxSpeed)
				elapsed := time.Since(lastUpdate)
				if elapsed < expectedTime {
					time.Sleep(expectedTime - elapsed)
				}
			}

			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				return totalWritten, writeErr
			}
			totalWritten += int64(written)

			if d.MaxSize > 0 && totalWritten > d.MaxSize {
				return totalWritten, fmt.Errorf("file size exceeded maximum allowed %s", formatBytes(d.MaxSize))
			}

			if time.Since(lastUpdate) >= time.Second {
				d.printProgress(totalWritten, totalSize, startTime, lastBytes)
				lastUpdate = time.Now()
				lastBytes = totalWritten
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return totalWritten, err
		}

		select {
		case <-ctx.Done():
			return totalWritten, ctx.Err()
		default:
		}
	}

	d.printProgress(totalWritten, totalSize, startTime, lastBytes)
	fmt.Println()

	if totalSize > 0 && totalWritten != totalSize {
		return totalWritten, fmt.Errorf("incomplete download: %s of %s",
			formatBytes(totalWritten), formatBytes(totalSize))
	}

	return totalWritten, nil
}

func (d *Downloader) printProgress(downloaded, total int64, startTime time.Time, lastBytes int64) {
	elapsed := time.Since(startTime).Seconds()
	percent := float64(0)
	if total > 0 {
		percent = float64(downloaded) / float64(total) * 100
	}

	speed := float64(0)
	if elapsed > 0 {
		speed = float64(downloaded-lastBytes) / time.Since(startTime).Seconds()
	}

	eta := time.Duration(0)
	if speed > 0 && total > 0 {
		remaining := float64(total-downloaded) / speed
		eta = time.Duration(remaining) * time.Second
	}

	progressBar := getProgressBar(percent, 30)
	downloadedStr := formatBytes(downloaded)
	totalStr := "Unknown"
	if total > 0 {
		totalStr = formatBytes(total)
	}
	speedStr := formatBytes(int64(speed)) + "/s"
	etaStr := formatTime(eta)

	fmt.Printf("\r%s %.1f%% | %s/%s | Speed: %s | ETA: %s",
		progressBar, percent, downloadedStr, totalStr, speedStr, etaStr)
}

func getProgressBar(percent float64, width int) string {
	completed := int((percent / 100) * float64(width))
	if completed > width {
		completed = width
	}
	if completed < 0 {
		completed = 0
	}
	remaining := width - completed
	return "[" + strings.Repeat("=", completed) + strings.Repeat(" ", remaining) + "]"
}

func formatBytes(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}

	units := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB"}
	base := float64(1024)
	exp := int(0)
	bytesFloat := float64(bytes)

	for bytesFloat >= base && exp < len(units)-1 {
		bytesFloat /= base
		exp++
	}

	return fmt.Sprintf("%.1f %s", bytesFloat, units[exp])
}

func formatTime(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, seconds)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func (d *Downloader) Cleanup(hours int) (int, error) {
	files, err := os.ReadDir(d.DownloadDir)
	if err != nil {
		return 0, err
	}

	cleaned := 0
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		info, err := file.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			filepath := filepath.Join(d.DownloadDir, file.Name())
			if err := os.Remove(filepath); err == nil {
				cleaned++
			}
		}
	}

	return cleaned, nil
}

func (d *Downloader) GetStats() (int, int64, error) {
	files, err := os.ReadDir(d.DownloadDir)
	if err != nil {
		return 0, 0, err
	}

	totalFiles := 0
	totalSize := int64(0)

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		info, err := file.Info()
		if err != nil {
			continue
		}

		totalFiles++
		totalSize += info.Size()
	}

	return totalFiles, totalSize, nil
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}

	downloader := NewDownloader(".")

	switch os.Args[1] {
	case "--help", "-h":
		printHelp()
	case "--stats":
		files, size, err := downloader.GetStats()
		if err != nil {
			fmt.Printf("Error getting stats: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Download Statistics:\n")
		fmt.Printf("  Total Files: %d\n", files)
		fmt.Printf("  Total Size: %s\n", formatBytes(size))
		fmt.Printf("  Download Directory: %s\n", downloader.DownloadDir)
	case "--cleanup":
		hours := 24
		if len(os.Args) > 2 {
			if h, err := strconv.Atoi(os.Args[2]); err == nil {
				hours = h
			}
		}
		cleaned, err := downloader.Cleanup(hours)
		if err != nil {
			fmt.Printf("Error during cleanup: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Cleaned up %d files older than %d hours\n", cleaned, hours)
	default:
		urlStr := ""
		customFilename := ""

		for i := 1; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "-o":
				if i+1 < len(os.Args) {
					customFilename = os.Args[i+1]
					i++
				}
			case "--timeout":
				if i+1 < len(os.Args) {
					if timeout, err := strconv.Atoi(os.Args[i+1]); err == nil {
						downloader.SetTimeout(time.Duration(timeout) * time.Second)
						i++
					}
				}
			case "--max-size":
				if i+1 < len(os.Args) {
					if size, err := strconv.ParseInt(os.Args[i+1], 10, 64); err == nil {
						downloader.SetMaxSize(size)
						i++
					}
				}
			case "--max-speed":
				if i+1 < len(os.Args) {
					if speed, err := strconv.ParseInt(os.Args[i+1], 10, 64); err == nil {
						downloader.SetMaxSpeed(speed)
						i++
					}
				}
			case "--no-ssl-verify":
				downloader.SetVerifySSL(false)
			default:
				if !strings.HasPrefix(os.Args[i], "-") && urlStr == "" {
					urlStr = os.Args[i]
				}
			}
		}

		if urlStr == "" {
			fmt.Printf("Error: No URL provided\n")
			printHelp()
			os.Exit(1)
		}

		result := downloader.Download(urlStr, customFilename)

		if result.Success {
			fmt.Printf("\nDownload completed successfully!\n")
			fmt.Printf("File: %s\n", result.Filename)
			fmt.Printf("Size: %s\n", formatBytes(result.Size))
			fmt.Printf("Time: %s\n", formatTime(result.DownloadTime))
			fmt.Printf("Average Speed: %s/s\n", formatBytes(int64(result.AverageSpeed)))
			fmt.Printf("Location: %s\n", result.Filepath)
			if result.Resumed {
				fmt.Printf("Note: Download was resumed\n")
			}
			if result.Retries > 0 {
				fmt.Printf("Retries: %d\n", result.Retries)
			}
		} else {
			fmt.Printf("\nDownload failed: %s\n", result.Error)
			os.Exit(1)
		}
	}
}

func printHelp() {
	fmt.Printf("URL File Downloader - Enhanced Downloader\n")
	fmt.Printf("Usage: %s [OPTIONS] URL\n\n", os.Args[0])
	fmt.Printf("Options:\n")
	fmt.Printf("  -o FILENAME          Output filename\n")
	fmt.Printf("  --timeout SECONDS    Overall timeout in seconds\n")
	fmt.Printf("  --max-size BYTES     Maximum file size in bytes\n")
	fmt.Printf("  --max-speed BYTES    Maximum download speed in bytes/sec\n")
	fmt.Printf("  --no-ssl-verify      Disable SSL certificate verification\n")
	fmt.Printf("  --stats              Show download statistics\n")
	fmt.Printf("  --cleanup [HOURS]    Cleanup old files (default: 24 hours)\n")
	fmt.Printf("  --help, -h           Show this help message\n")
}
