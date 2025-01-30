# Usage

```
$ burstshark --help

BurstShark is a network traffic analysis tool that wraps around tshark to identify and analyze bursty application data traffic, such as adaptive streaming, in real-time or from pcap files.

Usage: burstshark [OPTIONS]

Options:
  -i, --interface <INTERFACE>
          Network interface to use for live capture.
          
          Uses first non-loopback interface if no interface or file supplied.

  -f, --capture-filter <CAPTURE_FILTER>
          Packet filter for live capture in libpcap filter syntax.
          
          Merged with a default filter that captures UDP and TCP packets with payload, or QoS data WLAN frames if WLAN is enabled.

  -s, --snapshot-length <SNAPLEN>
          Number of bytes to capture per packet during live capture.
          
          No more than snaplen bytes of each packet will be read into memory, or saved. The default value is configured to capture relevant headers / packet length information required for BurstShark. A snaplen of 0 will capture the entire packet.
          
          [default: 96]

  -r, --read-file <INFILE>
          Read packet data from infile.
          
          Can be any capture file format supported by tshark, including gzipped files.

  -Y, --display-filter <DISPLAY_FILTER>
          Packet filter in Wireshark display filter syntax.
          
          Can be used for both live capture and reading from a file. Less efficient than a capture filter for live capture so it is recommended to move as much of the filtering logic as possible to the capture filter.

  -t, --burst_timeout <BURST_TIMEOUT>
          Seconds with no flow activity for a burst to be considered complete
          
          [default: 0.5]

  -a, --aggregate-ports
          Aggregate ports for flows with the same IP src/dst pair to a single flow.
          
          If enabled, output bursts will have a source and destination port of 0.

  -w, --write-pcap <PCAP_OUTFILE>
          Write raw packet data read by tshark to pcap_outfile

  -b, --min-bytes <MIN_BYTES>
          Only display bursts with a minimum size of min_bytes

  -B, --max-bytes <MAX_BYTES>
          Only display bursts with a maximum size of max_bytes

  -p, --min-packets <MIN_PACKETS>
          Only display bursts with a minimum amount of min_packets packets/frames

  -P, --max-packets <MAX_PACKETS>
          Only display bursts with a maximum amount of max_packets packets/frames

  -I, --wlan
          Read 802.11 WLAN QoS data frames instead of IP packets.
          
          For live capture, the interface should be in monitor mode.

  -E, --no-estimation
          Disable frame size estimation for missed WLAN frames.
          
          By default, missed WLAN frames will have their sizes estimated based on the average size of the frames captured.

  -M, --max-deviation <MAX_DEVIATION>
          Maximum allowed deviation from the expected WLAN sequence number.
          
          Only frames within max_deviation will be considered and estimated.
          
          [default: 500]

  -h, --help
          Print help (see a summary with '-h')

  -V, --version
          Print version
```

