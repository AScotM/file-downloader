package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Downloader struct {
	DownloadDir string
	Timeout     time.Duration
	UserAgent   string
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
}

func NewDownloader(downloadDir string, timeout time.Duration) *Downloader {
	if downloadDir == "" {
		downloadDir = "."
	}

	return &Downloader{
		DownloadDir: downloadDir,
		Timeout:     timeout,
		UserAgent:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
	}
}

func (d *Downloader) Download(urlStr, customFilename string) *DownloadResult {
	startTime := time.Now()
	result := &DownloadResult{URL: urlStr}

	if err := d.validateURL(urlStr); err != nil {
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
	filepath := filepath.Join(d.DownloadDir, filename)

	resumeFrom := int64(0)
	if d.canResume(filepath) {
		if info, err := os.Stat(filepath); err == nil {
			resumeFrom = info.Size()
			result.Resumed = true
		}
	}

	size, err := d.downloadFile(urlStr, filepath, resumeFrom)
	if err != nil {
		result.Error = err.Error()
		if resumeFrom == 0 {
			os.Remove(filepath)
		}
		return result
	}

	result.Success = true
	result.Filename = filename
	result.Filepath = filepath
	result.Size = size
	result.DownloadTime = time.Since(startTime)
	if result.DownloadTime > 0 {
		result.AverageSpeed = float64(size) / result.DownloadTime.Seconds()
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

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("HTTP error: %s", resp.Status)
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

	reader := bufio.NewReader(resp.Body)
	buffer := make([]byte, 32*1024)
	var totalWritten int64 = resumeFrom
	lastUpdate := time.Now()
	startTime := time.Now()

	fmt.Printf("Downloading: %s\n", urlStr)
	if resumeFrom > 0 {
		fmt.Printf("Resuming from: %s\n", formatBytes(resumeFrom))
	}

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				return totalWritten, writeErr
			}
			totalWritten += int64(written)

			if time.Since(lastUpdate) >= time.Second {
				d.printProgress(totalWritten, totalSize, startTime)
				lastUpdate = time.Now()
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

	d.printProgress(totalWritten, totalSize, startTime)
	fmt.Println()

	return totalWritten, nil
}

func (d *Downloader) printProgress(downloaded, total int64, startTime time.Time) {
	elapsed := time.Since(startTime).Seconds()
	percent := float64(0)
	if total > 0 {
		percent = float64(downloaded) / float64(total) * 100
	}

	speed := float64(0)
	if elapsed > 0 {
		speed = float64(downloaded) / elapsed
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

	downloader := NewDownloader(".", 0)

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
			if os.Args[i] == "-o" && i+1 < len(os.Args) {
				customFilename = os.Args[i+1]
				i++
			} else if strings.HasPrefix(os.Args[i], "-") {
				continue
			} else if urlStr == "" {
				urlStr = os.Args[i]
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
		} else {
			fmt.Printf("\nDownload failed: %s\n", result.Error)
			os.Exit(1)
		}
	}
}

func printHelp() {
	fmt.Printf("URL File Downloader - Unlimited Downloader\n")
	fmt.Printf("Usage: %s [OPTIONS] URL\n\n", os.Args[0])
	fmt.Printf("Options:\n")
	fmt.Printf("  -o FILENAME          Output filename\n")
	fmt.Printf("  --stats              Show download statistics\n")
	fmt.Printf("  --cleanup [HOURS]    Cleanup old files (default: 24 hours)\n")
	fmt.Printf("  --help, -h           Show this help message\n")
}
