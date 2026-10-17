use std::sync::{Arc, Mutex};

use bytes::Bytes;
use hang::catalog::Container;
use mp4_atom::{Atom, DecodeAtom, DecodeMaybe, Header, Mdat, Moof};

use super::import::{Rendition, init_audio, init_video};
use super::{Error, Kind, Wire};
use crate::Result;
use crate::catalog::hang::CatalogExt;

/// Publishes one CMAF track whose fragments go out byte for byte, advertised in the catalog.
///
/// [`Import`](super::Import) re-encodes every fragment (rewriting `tfdt` and data offsets) and drops
/// top-level boxes it doesn't know, which breaks anything signed over the fragment's bytes. This
/// writes each fragment as one frame, unchanged, and leaves grouping to the caller.
///
/// Like the importer, it holds its reservation until the first fragment anchors the
/// [`Timebase`](crate::catalog::Timebase), so the first catalog carries the final clock. Unlike it,
/// the track is not enrolled in the broadcast's timeline, and its rendition carries no measured
/// bitrate or jitter. The catalog entry lasts as long as this producer.
pub struct Passthrough<E: CatalogExt = ()> {
	track: moq_net::track::Producer,
	stamp: Stamp<E>,
	// Retires the catalog entry on drop.
	_rendition: Rendition<E>,
}

impl<E: CatalogExt> Passthrough<E> {
	/// Accept `request` as a track described by `init`.
	///
	/// `init` is a CMAF init segment (ftyp+moov with exactly one trak). The rendition's container
	/// init is `init` verbatim; the rest of it (codec, description, dimensions, sample rate,
	/// channels) is derived from the trak the way [`Import`](super::Import) derives it, and the
	/// track runs at the trak's `mdhd` timescale.
	pub fn new(request: moq_net::track::Request, reserved: crate::catalog::Reserved<E>, init: Bytes) -> Result<Self> {
		let wire = Wire::from_init(&init)?;
		let trak = wire.trak();
		let kind = Kind::from_handler(trak.mdia.hdlr.handler)?;
		let timescale = moq_net::Timescale::new(trak.mdia.mdhd.timescale as u64)?;

		let track = request.accept(reserved.track_info(kind.priority()).with_timescale(timescale));
		let container = Container::Cmaf { init };
		let rendition = match kind {
			Kind::Video => {
				let mut rendition = reserved.init(track.name())?;
				rendition.set(init_video(trak, container)?)?;
				Rendition::Video(rendition)
			}
			Kind::Audio => {
				let mut rendition = reserved.init(track.name())?;
				rendition.set(init_audio(trak, container)?)?;
				Rendition::Audio(rendition)
			}
		};

		Ok(Self {
			track,
			stamp: Stamp {
				track_id: trak.tkhd.track_id,
				timescale,
				timebase: reserved.timebase(),
				reservation: Arc::new(Mutex::new(Some(reserved))),
			},
			_rendition: rendition,
		})
	}

	/// The track name, which is also the rendition's catalog key.
	pub fn name(&self) -> &str {
		self.track.name()
	}

	/// Open the group numbered `sequence`.
	pub fn create_group(&mut self, sequence: u64) -> Result<PassthroughGroup<E>> {
		let group = self.track.create_group(moq_net::group::Info { sequence })?;
		Ok(self.group(group))
	}

	/// Open the group after the latest one.
	pub fn append_group(&mut self) -> Result<PassthroughGroup<E>> {
		let group = self.track.append_group()?;
		Ok(self.group(group))
	}

	fn group(&self, group: moq_net::group::Producer) -> PassthroughGroup<E> {
		PassthroughGroup {
			group,
			stamp: self.stamp.clone(),
		}
	}

	/// End the track at the live edge. Retires the catalog entry.
	pub fn finish(self) -> Result<()> {
		// Nothing more will anchor the clock, so stop withholding the catalog.
		self.stamp.reservation.lock().unwrap().take();
		self.track.finish()?;
		Ok(())
	}

	/// End the track with `err`. Retires the catalog entry.
	pub fn abort(self, err: moq_net::Error) -> Result<()> {
		self.track.abort(err)?;
		Ok(())
	}
}

/// What a group needs to stamp a fragment, shared by a track and its groups.
struct Stamp<E: CatalogExt> {
	track_id: u32,
	timescale: moq_net::Timescale,
	timebase: crate::catalog::Timebase<E>,
	/// Withholds the catalog until the first fragment anchors the timebase.
	reservation: Arc<Mutex<Option<crate::catalog::Reserved<E>>>>,
}

// Manual so a stamp is clonable regardless of whether `E` is.
impl<E: CatalogExt> Clone for Stamp<E> {
	fn clone(&self) -> Self {
		Self {
			track_id: self.track_id,
			timescale: self.timescale,
			timebase: self.timebase.clone(),
			reservation: self.reservation.clone(),
		}
	}
}

/// One group of a [`Passthrough`] track, where each fragment is one frame.
pub struct PassthroughGroup<E: CatalogExt = ()> {
	group: moq_net::group::Producer,
	stamp: Stamp<E>,
}

impl<E: CatalogExt> PassthroughGroup<E> {
	/// The group's sequence number within the track.
	pub fn sequence(&self) -> u64 {
		self.group.sequence
	}

	/// Write one fragment as one frame, unchanged.
	///
	/// A fragment is any leading top-level boxes (e.g. `uuid`), then exactly one `moof` and one
	/// `mdat`. The `moof` holds a single `traf` for this track. The frame timestamp is the first
	/// sample's presentation time (`tfdt` plus the first `trun` entry's composition offset) on the
	/// track's timescale, shifted onto the catalog clock by the timebase.
	pub fn write(&mut self, fragment: Bytes) -> Result<()> {
		let pts = self.pts(&fragment)?;
		let timestamp = self.stamp.timebase.shift(pts)?;
		self.stamp.reservation.lock().unwrap().take();
		self.group.write_frame(timestamp, fragment)?;
		Ok(())
	}

	fn pts(&self, fragment: &[u8]) -> Result<moq_net::Timestamp> {
		let mut buf = fragment;
		let mut moof = None;
		let mut mdat = false;

		// Walk the box headers rather than decoding every box, so the mdat payload isn't copied.
		while !buf.is_empty() {
			let header = Header::decode_maybe(&mut buf)?.ok_or(Error::TruncatedFragment)?;
			let size = header.size.unwrap_or(buf.len());
			if size > buf.len() {
				return Err(Error::TruncatedFragment.into());
			}
			let (mut body, rest) = buf.split_at(size);
			buf = rest;

			match header.kind {
				Moof::KIND if moof.is_none() => moof = Some(Moof::decode_atom(&header, &mut body)?),
				Mdat::KIND if moof.is_some() && !mdat => mdat = true,
				kind if moof.is_none() && kind != Mdat::KIND => {}
				kind => return Err(Error::UnexpectedFragmentBox(kind).into()),
			}
		}

		let moof = moof.ok_or(Error::NoMoof)?;
		if !mdat {
			return Err(Error::NoMdat.into());
		}

		let traf = match moof.traf.as_slice() {
			[traf] => traf,
			[] => return Err(Error::NoTraf.into()),
			trafs => return Err(Error::MultipleTrafs(trafs.len()).into()),
		};
		if traf.tfhd.track_id != self.stamp.track_id {
			return Err(Error::UnknownTrack(traf.tfhd.track_id).into());
		}

		let tfdt = traf.tfdt.as_ref().ok_or(Error::NoTfdt)?;
		let first = traf
			.trun
			.iter()
			.find_map(|trun| trun.entries.first())
			.ok_or(Error::MissingTrun)?;
		let pts = tfdt
			.base_media_decode_time
			.checked_add_signed(first.cts.unwrap_or_default() as i64)
			.ok_or(Error::PtsOverflow)?;

		Ok(moq_net::Timestamp::new(pts, self.stamp.timescale)?)
	}

	/// Mark the group complete.
	pub fn finish(&mut self) -> Result<()> {
		self.group.finish()?;
		Ok(())
	}

	/// End the group with `err`.
	pub fn abort(self, err: moq_net::Error) -> Result<()> {
		self.group.abort(err)?;
		Ok(())
	}
}

#[cfg(test)]
mod tests {
	use super::*;
	use mp4_atom::{Decode, Encode};

	const H264_INIT: &[u8] = include_bytes!("test_data/muxl-h264-init.mp4");
	const OPUS_INIT: &[u8] = include_bytes!("test_data/muxl-opus-init.mp4");
	const AAC_INIT: &[u8] = include_bytes!("test_data/muxl-aac-init.mp4");
	// One MUXL Opus fragment (tfdt 10661430 at 48 kHz, no composition offset), as published.
	const OPUS_FRAGMENT: &[u8] = include_bytes!("test_data/muxl-opus-fragment.mp4");

	fn publish(init: &[u8]) -> (moq_net::broadcast::Producer, crate::catalog::Producer, Passthrough) {
		let mut broadcast = moq_net::broadcast::Info::new().produce();
		let catalog = crate::catalog::Producer::new(&mut broadcast, crate::catalog::Config::default()).unwrap();
		let request = broadcast.reserve_track("track").unwrap();
		let track = Passthrough::new(request, catalog.reserve(), Bytes::copy_from_slice(init)).unwrap();
		(broadcast, catalog, track)
	}

	fn uuid_box() -> Vec<u8> {
		let mut buf = Vec::new();
		mp4_atom::Any::Unknown(mp4_atom::FourCC::new(b"uuid"), [7u8; 20].to_vec())
			.encode(&mut buf)
			.unwrap();
		buf
	}

	#[test]
	fn rendition_comes_from_init() {
		let (_broadcast, catalog, _track) = publish(H264_INIT);
		let video = catalog.snapshot().video.renditions["track"].clone();
		assert_eq!(video.codec.to_string(), "avc1.64002a");
		assert_eq!((video.coded_width, video.coded_height), (Some(1920), Some(1080)));
		// The avcC payload (version 1, then profile and level), not Annex B.
		assert_eq!(video.description.unwrap()[..4], [1, 0x64, 0x00, 0x2a]);
		assert_eq!(video.container, Container::Cmaf { init: H264_INIT.into() });

		let (_broadcast, catalog, _track) = publish(OPUS_INIT);
		let audio = catalog.snapshot().audio.renditions["track"].clone();
		assert_eq!(audio.codec.to_string(), "opus");
		assert_eq!((audio.sample_rate, audio.channel_count), (48_000, 2));
		assert!(audio.description.unwrap().starts_with(b"OpusHead"));
		assert_eq!(audio.container, Container::Cmaf { init: OPUS_INIT.into() });

		let (_broadcast, catalog, track) = publish(AAC_INIT);
		let audio = catalog.snapshot().audio.renditions["track"].clone();
		assert_eq!(audio.codec.to_string(), "mp4a.40.2");
		assert_eq!((audio.sample_rate, audio.channel_count), (48_000, 2));
		// The raw AudioSpecificConfig: AAC-LC at 48 kHz, stereo.
		assert_eq!(audio.description.unwrap().as_ref(), &[0x11, 0x90]);
		assert_eq!(audio.container, Container::Cmaf { init: AAC_INIT.into() });

		// The entry lasts as long as the producer.
		drop(track);
		assert!(catalog.snapshot().audio.renditions.is_empty());
	}

	#[tokio::test]
	async fn fragment_passes_through_unchanged() {
		let (broadcast, _catalog, mut track) = publish(OPUS_INIT);
		let mut subscriber = broadcast
			.consume()
			.track("track")
			.unwrap()
			.subscribe(None)
			.await
			.unwrap();

		let fragment: Bytes = [uuid_box().as_slice(), OPUS_FRAGMENT].concat().into();
		let mut group = track.create_group(5).unwrap();
		assert_eq!(group.sequence(), 5);
		group.write(fragment.clone()).unwrap();
		group.write(with_cts(-480)).unwrap();
		group.finish().unwrap();

		// The first fragment places the catalog clock, so its timestamp stays verbatim.
		let timescale = moq_net::Timescale::new(48_000).unwrap();
		let mut group = subscriber.recv_group().await.unwrap().unwrap();
		assert_eq!(group.sequence, 5);
		let frame = group.read_frame().await.unwrap().unwrap();
		assert_eq!(frame.payload, fragment);
		assert_eq!(
			frame.timestamp,
			Some(moq_net::Timestamp::new(10_661_430, timescale).unwrap())
		);
		// The first sample's presentation time, not its decode time.
		let frame = group.read_frame().await.unwrap().unwrap();
		assert_eq!(
			frame.timestamp,
			Some(moq_net::Timestamp::new(10_661_430 - 480, timescale).unwrap())
		);
	}

	/// The Opus fragment with a composition offset on its sample, its data offset re-pointed.
	fn with_cts(cts: i32) -> Bytes {
		let mut buf = OPUS_FRAGMENT;
		let mut moof = Moof::decode(&mut buf).unwrap();
		moof.traf[0].trun[0].entries[0].cts = Some(cts);
		let mut out = Vec::new();
		moof.encode(&mut out).unwrap();
		moof.traf[0].trun[0].data_offset = Some(out.len() as i32 + 8);
		out.clear();
		moof.encode(&mut out).unwrap();
		out.extend_from_slice(buf);
		out.into()
	}

	#[test]
	fn malformed_fragments_are_refused() {
		let (_broadcast, _catalog, mut track) = publish(OPUS_INIT);
		let mut group = track.append_group().unwrap();

		// The fragment is moof then mdat; split it to rearrange.
		let moof_size = u32::from_be_bytes(OPUS_FRAGMENT[..4].try_into().unwrap()) as usize;
		let (moof, mdat) = OPUS_FRAGMENT.split_at(moof_size);
		let uuid = uuid_box();

		for (name, fragment) in [
			("mdat first", [mdat, moof].concat()),
			("two moofs", [moof, moof, mdat].concat()),
			("no mdat", moof.to_vec()),
			("trailing box", [moof, mdat, &uuid].concat()),
			("truncated", OPUS_FRAGMENT[..OPUS_FRAGMENT.len() - 1].to_vec()),
		] {
			assert!(group.write(fragment.into()).is_err(), "{name} was accepted");
		}

		// A fragment from another track of the same source.
		let (_broadcast, _catalog, mut video) = publish(H264_INIT);
		let mut group = video.append_group().unwrap();
		assert!(matches!(
			group.write(Bytes::from_static(OPUS_FRAGMENT)),
			Err(crate::Error::Cmaf(Error::UnknownTrack(_)))
		));
	}
}
