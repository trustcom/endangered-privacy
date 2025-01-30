use std::error::Error;

use clap::Parser;

use burstshark::capture::{CaptureType, CommonOptions};
use burstshark::output::OutputWriter;

#[derive(Parser, Clone, Debug)]
#[clap(author, version, about)]
struct Args {
    /// Network interface to use for live capture.
    ///
    /// Uses first non-loopback interface if no interface or file supplied.
    #[clap(short = 'i', long = "interface")]
    interface: Option<String>,

    /// Packet filter for live capture in libpcap filter syntax.
    ///
    /// Merged with a default filter that captures UDP and TCP packets with payload,
    /// or QoS data WLAN frames if WLAN is enabled.
    #[clap(short = 'f', long = "capture-filter", conflicts_with = "infile")]
    capture_filter: Option<String>,

    /// Number of bytes to capture per packet during live capture.
    ///
    /// No more than snaplen bytes of each packet will be read into memory, or saved. The
    /// default value is configured to capture relevant headers / packet length information
    /// required for BurstShark. A snaplen of 0 will capture the entire packet.
    #[clap(
        short = 's',
        long = "snapshot-length",
        default_value_t = 96,
        conflicts_with = "infile"
    )]
    snaplen: u32,

    /// Read packet data from infile.
    ///
    /// Can be any capture file format supported by tshark, including gzipped files.
    #[clap(short = 'r', long = "read-file", conflicts_with = "interface")]
    infile: Option<String>,

    /// Packet filter in Wireshark display filter syntax.
    ///
    /// Can be used for both live capture and reading from a file. Less efficient than a
    /// capture filter for live capture so it is recommended to move as much of the
    /// filtering logic as possible to the capture filter.
    #[clap(short = 'Y', long = "display-filter")]
    display_filter: Option<String>,

    /// Seconds with no flow activity for a burst to be considered complete.
    #[clap(short = 't', long = "burst_timeout", default_value_t = 0.5)]
    burst_timeout: f64,

    /// Aggregate ports for flows with the same IP src/dst pair to a single flow.
    ///
    /// If enabled, output bursts will have a source and destination port of 0.
    #[clap(short = 'a', long = "aggregate-ports", conflicts_with = "wlan")]
    aggregate_ports: bool,

    /// Write raw packet data read by tshark to pcap_outfile.
    #[clap(short = 'w', long = "write-pcap")]
    pcap_outfile: Option<String>,

    /// Only display bursts with a minimum size of min_bytes.
    #[clap(short = 'b', long = "min-bytes")]
    min_bytes: Option<u32>,

    /// Only display bursts with a maximum size of max_bytes.
    #[clap(short = 'B', long = "max-bytes")]
    max_bytes: Option<u32>,

    /// Only display bursts with a minimum amount of min_packets packets/frames.
    #[clap(short = 'p', long = "min-packets")]
    min_packets: Option<u16>,

    /// Only display bursts with a maximum amount of max_packets packets/frames.
    #[clap(short = 'P', long = "max-packets")]
    max_packets: Option<u16>,

    /// Read 802.11 WLAN QoS data frames instead of IP packets.
    ///
    /// For live capture, the interface should be in monitor mode.
    #[clap(short = 'I', long = "wlan")]
    wlan: bool,

    /// Disable frame size estimation for missed WLAN frames.
    ///
    /// By default, missed WLAN frames will have their sizes estimated based on the
    /// average size of the frames captured.
    #[clap(short = 'E', long = "no-estimation", requires = "wlan")]
    no_estimation: bool,

    /// Maximum allowed deviation from the expected WLAN sequence number.
    ///
    /// Only frames within max_deviation will be considered and estimated.
    #[clap(
        short = 'M',
        long = "max-deviation",
        default_value_t = 500,
        requires = "wlan"
    )]
    max_deviation: u16,

    #[clap(value_delimiter=' ', hide(true), conflicts_with_all(["capture_filter", "display_filter"]))]
    positional_filter: Option<Vec<String>>,
}

fn tshark_args(args: Args) -> Vec<String> {
    let mut tshark_args = vec!["-l", "-q", "-n", "-T", "fields"];

    #[rustfmt::skip]
    tshark_args.extend(match args.wlan {
        false => vec![
            "-e", "frame.time_epoch",
            "-e", "ip.src",
            "-e", "ipv6.src",
            "-e", "ip.dst",
            "-e", "ipv6.dst",
            "-e", "data.len",
            "-e", "udp.length",
            "-e", "tcp.len",
            "-e", "udp.srcport",
            "-e", "tcp.srcport",
            "-e", "udp.dstport",
            "-e", "tcp.dstport",
        ],
        true => vec![
            "-e", "frame.time_epoch",
            "-e", "wlan.sa",
            "-e", "wlan.da",
            "-e", "frame.len",
            "-e", "wlan.seq",
        ],
    });

    let base_filter = match (&args.infile, &args.wlan) {
        (None, false) => String::from(
            "udp or (tcp and (((ip[2:2] - ((ip[0]&0xf)<<2)) - ((tcp[12]&0xf0)>>2)) != 0))",
        ),
        (None, true) => String::from("wlan type data subtype qos-data"),
        (Some(_), false) => String::from("udp or (tcp and tcp.len > 0)"),
        (Some(_), true) => String::from("wlan and wlan.fc.type_subtype == 40"),
    };

    let create_filter = |optional_filter: Option<String>| -> Option<String> {
        match optional_filter.or(args.positional_filter.map(|f| f.join(" "))) {
            Some(filter) => Some(format!("({}) and ({})", base_filter, filter)),
            None => Some(base_filter),
        }
    };

    let (capture_filter, display_filter) = match &args.infile {
        None => (create_filter(args.capture_filter), args.display_filter),
        Some(_) => (None, create_filter(args.display_filter)),
    };

    let snapshot_length = args.snaplen.to_string();

    if let Some(capture_filter) = &capture_filter {
        tshark_args.extend(vec!["-s", &snapshot_length, "-f", capture_filter]);
    }

    if let Some(display_filter) = &display_filter {
        tshark_args.extend(vec!["-Y", display_filter]);
    }

    if let Some(interface) = &args.interface {
        tshark_args.extend(vec!["-i", interface]);
    }

    if let Some(infile) = &args.infile {
        tshark_args.extend(vec!["-r", infile]);
    }

    if let Some(pcap_outfile) = &args.pcap_outfile {
        tshark_args.extend(vec!["-w", pcap_outfile, "-P"]);
    }

    tshark_args.into_iter().map(str::to_string).collect()
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let args: Args = Args::parse();

    let output_tx = OutputWriter::new(
        args.min_bytes,
        args.max_bytes,
        args.min_packets,
        args.max_packets,
    )
    .start()
    .await;

    let opts = CommonOptions {
        tshark_args: tshark_args(args.clone()),
        burst_timeout: args.burst_timeout,
        output_tx,
    };

    match args.wlan {
        false => CaptureType::Ip {
            opts,
            aggregate_ports: args.aggregate_ports,
        },
        true => CaptureType::Wlan {
            opts,
            no_estimation: args.no_estimation,
            max_deviation: args.max_deviation,
        },
    }
    .run()
    .await?;

    Ok(())
}
