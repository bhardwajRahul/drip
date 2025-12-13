package stats

// FormatBytes formats bytes to human readable string
func FormatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return formatFloat(float64(bytes)/float64(GB)) + " GB"
	case bytes >= MB:
		return formatFloat(float64(bytes)/float64(MB)) + " MB"
	case bytes >= KB:
		return formatFloat(float64(bytes)/float64(KB)) + " KB"
	default:
		return formatInt(bytes) + " B"
	}
}

// FormatSpeed formats speed (bytes per second) to human readable string
func FormatSpeed(bytesPerSec int64) string {
	if bytesPerSec == 0 {
		return "0 B/s"
	}
	return FormatBytes(bytesPerSec) + "/s"
}

func formatFloat(f float64) string {
	if f >= 100 {
		return formatInt(int64(f))
	} else if f >= 10 {
		return formatOneDecimal(f)
	}
	return formatTwoDecimal(f)
}

func formatInt(i int64) string {
	return intToStr(i)
}

func formatOneDecimal(f float64) string {
	i := int64(f * 10)
	whole := i / 10
	frac := i % 10
	return intToStr(whole) + "." + intToStr(frac)
}

func formatTwoDecimal(f float64) string {
	i := int64(f * 100)
	whole := i / 100
	frac := i % 100
	if frac < 10 {
		return intToStr(whole) + ".0" + intToStr(frac)
	}
	return intToStr(whole) + "." + intToStr(frac)
}

func intToStr(i int64) string {
	if i == 0 {
		return "0"
	}
	if i < 0 {
		return "-" + intToStr(-i)
	}

	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
