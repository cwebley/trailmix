import AVFoundation
import CoreGraphics
import CoreMedia
import Foundation
import ScreenCaptureKit

struct CaptureError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
    init(_ message: String) { self.message = message }
}

func jsonLine(_ value: [String: Any]) throws -> Data {
    var data = try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
    data.append(0x0A)
    return data
}

func emit(_ value: [String: Any]) {
    do {
        try FileHandle.standardOutput.write(contentsOf: jsonLine(value))
    } catch {
        // A lost parent must not leave an invisible recording running.
        exit(1)
    }
}

func microphones() -> [AVCaptureDevice] {
    AVCaptureDevice.DiscoverySession(
        deviceTypes: [.microphone, .external], mediaType: .audio, position: .unspecified
    ).devices
}

func exclusiveFile(_ url: URL) throws -> FileHandle {
    let fd = open(url.path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
    guard fd >= 0 else { throw CaptureError("Cannot create \(url.path): \(String(cString: strerror(errno)))") }
    return FileHandle(fileDescriptor: fd, closeOnDealloc: true)
}

func pcmPeak(_ pcm: AVAudioPCMBuffer) throws -> Float {
    func maximum<T>(_ type: T.Type, normalize: (T) -> Double) -> Float {
        var peak: Double = 0
        for buffer in UnsafeMutableAudioBufferListPointer(pcm.mutableAudioBufferList) {
            guard let data = buffer.mData else { continue }
            let samples = data.assumingMemoryBound(to: T.self)
            for index in 0..<(Int(buffer.mDataByteSize) / MemoryLayout<T>.stride) {
                peak = max(peak, abs(normalize(samples[index])))
            }
        }
        return Float(peak)
    }
    switch pcm.format.commonFormat {
    case .pcmFormatFloat32: return maximum(Float.self) { Double($0) }
    case .pcmFormatFloat64: return maximum(Double.self) { $0 }
    case .pcmFormatInt16: return maximum(Int16.self) { Double($0) / 32768 }
    case .pcmFormatInt32: return maximum(Int32.self) { Double($0) / 2147483648 }
    default: throw CaptureError("Unsupported PCM format for audio level measurement")
    }
}

// All accesses to TrackWriter and CaptureOutput's mutable state use output.queue.
// CAF stores the original PCM format. The JSONL maps file frames to meeting time,
// retaining gaps instead of silently treating concatenated buffers as continuous.
final class TrackWriter {
    let name: String
    let directory: URL
    let timeline: FileHandle
    var file: AVAudioFile?
    var format: AVAudioFormat?
    var frames: Int64 = 0
    var firstSeconds: Double = 0
    var endSeconds: Double = 0
    var discontinuities = 0
    var peak: Float = 0
    var peakMax: Float = 0
    var stream: StreamWriter?
    var streamError: String?

    init(name: String, directory: URL, streaming: Bool = false) throws {
        self.name = name
        self.directory = directory
        timeline = try exclusiveFile(directory.appendingPathComponent("\(name).timeline.jsonl"))
        if streaming {
            do { stream = try StreamWriter(name: name, directory: directory) }
            catch { reportStreamError(error) }
        }
    }

    func reportStreamError(_ error: any Error) {
        streamError = error.localizedDescription
        if let marker = try? exclusiveFile(directory.appendingPathComponent("\(name).stream.error")) {
            try? marker.write(contentsOf: Data(error.localizedDescription.utf8))
            try? marker.close()
        }
        emit(["type": "transcription_error", "error": "\(name): \(error.localizedDescription)"])
    }

    func write(_ sample: CMSampleBuffer, startSeconds: Double) throws {
        guard startSeconds.isFinite, let description = sample.formatDescription else {
            throw CaptureError("\(name): invalid audio format or timestamp")
        }
        let incoming = AVAudioFormat(cmAudioFormatDescription: description)
        guard incoming.sampleRate > 0, incoming.channelCount > 0 else {
            throw CaptureError("\(name): invalid sample rate or channel count")
        }
        let count = CMSampleBufferGetNumSamples(sample)
        guard count > 0 else { return }
        if let format, format != incoming {
            throw CaptureError("\(name): format changed from \(format) to \(incoming). Stop and restart with the new device.")
        }
        if file == nil {
            let url = directory.appendingPathComponent("\(name).caf")
            let reservation = try exclusiveFile(url)
            try reservation.close()
            var settings = incoming.settings
            settings[AVLinearPCMIsNonInterleaved] = false
            file = try AVAudioFile(
                forWriting: url, settings: settings,
                commonFormat: incoming.commonFormat, interleaved: incoming.isInterleaved
            )
            format = incoming
            firstSeconds = startSeconds
        }
        let buffers = AudioBufferList.allocate(maximumBuffers: Int(incoming.channelCount))
        defer { free(buffers.unsafeMutablePointer) }
        var block: CMBlockBuffer?
        let result = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
            sample, bufferListSizeNeededOut: nil,
            bufferListOut: buffers.unsafeMutablePointer,
            bufferListSize: MemoryLayout<AudioBufferList>.size
                + (Int(incoming.channelCount) - 1) * MemoryLayout<AudioBuffer>.stride,
            blockBufferAllocator: kCFAllocatorDefault,
            blockBufferMemoryAllocator: kCFAllocatorDefault,
            flags: kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment,
            blockBufferOut: &block
        )
        guard result == noErr else { throw CaptureError("\(name): cannot read PCM buffers (\(result))") }
        // Keep the retained block alive through the no-copy buffer's write.
        try withExtendedLifetime(block) {
            guard let pcm = AVAudioPCMBuffer(
                pcmFormat: incoming, bufferListNoCopy: buffers.unsafePointer, deallocator: nil
            ) else { throw CaptureError("\(name): unsupported PCM layout") }
            guard pcm.frameLength == count else { throw CaptureError("\(name): PCM frame count mismatch") }
            peak = try pcmPeak(pcm)
            peakMax = max(peakMax, peak)
            try file!.write(from: pcm)
            if let stream, streamError == nil {
                do { try stream.write(pcm, start: startSeconds) }
                catch { reportStreamError(error) }
            }
        }
        let duration = Double(count) / incoming.sampleRate
        let delta = frames == 0 ? 0 : startSeconds - endSeconds
        // Ignore sub-frame rounding; record gaps and overlaps larger than 2 ms.
        if abs(delta) > 0.002 { discontinuities += 1 }
        try timeline.write(contentsOf: jsonLine([
            "file_frame": frames,
            "frames": count,
            "start_seconds": startSeconds,
            "end_seconds": startSeconds + duration,
            "sample_rate": incoming.sampleRate,
            "channels": incoming.channelCount,
            "discontinuity_seconds": abs(delta) > 0.002 ? delta : 0,
        ]))
        frames += Int64(count)
        endSeconds = startSeconds + duration
    }

    var stats: [String: Any] {
        ["frames": frames, "sample_rate": format?.sampleRate ?? 0,
         "channels": format?.channelCount ?? 0, "first_seconds": firstSeconds,
         "end_seconds": endSeconds, "discontinuities": discontinuities, "peak": peak,
         "peak_max": peakMax]
    }

    func finish() throws {
        file = nil // AVAudioFile closes and finalizes its CAF header on release.
        try timeline.synchronize()
        try timeline.close()
        if let stream {
            do { if streamError == nil { try stream.finish() } else { try stream.abort() } }
            catch { reportStreamError(error) }
        }
    }
}

// A durable queue, not a pipe to the network. Publish a timing row only after its
// PCM bytes are written. Go can lag or restart without blocking native capture.
final class StreamWriter {
    let audio: FileHandle
    let timeline: FileHandle
    let outputFormat = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: 24_000, channels: 1, interleaved: true)!
    var converter: AVAudioConverter?
    var offset: Int64 = 0
    var previousEnd: Double?
    var inputFrames: Int64 = 0
    var outputFrames: Int64 = 0
    var pendingGap = false
    var pendingPCM = Data()
    struct Timing {
        let firstFrame: Int64
        let frames: Int64
        let start: Double
    }
    var timings: [Timing] = []

    init(name: String, directory: URL) throws {
        audio = try exclusiveFile(directory.appendingPathComponent("\(name).stream.pcm"))
        timeline = try exclusiveFile(directory.appendingPathComponent("\(name).stream.jsonl"))
    }

    func write(_ pcm: AVAudioPCMBuffer, start: Double) throws {
        let end = start + Double(pcm.frameLength) / pcm.format.sampleRate
        let gap = previousEnd.map { abs(start - $0) > 0.002 } ?? false
        if converter == nil || gap {
            try drain()
            converter = AVAudioConverter(from: pcm.format, to: outputFormat)
            // Normal priming compensates the resampler's filter delay. No-data
            // callbacks can postpone output, so timestamps follow output frames,
            // not the input buffer which happened to make output available.
            converter?.primeMethod = .normal
            converter?.downmix = true
            inputFrames = 0
            outputFrames = 0
            pendingGap = gap
            pendingPCM.removeAll(keepingCapacity: true)
            timings.removeAll(keepingCapacity: true)
        }
        guard let converter else { throw CaptureError("Cannot convert transcription PCM") }
        timings.append(Timing(firstFrame: inputFrames, frames: Int64(pcm.frameLength), start: start))
        inputFrames += Int64(pcm.frameLength)
        let capacity = AVAudioFrameCount(ceil(Double(pcm.frameLength) * 24_000 / pcm.format.sampleRate)) + 64
        let output = AVAudioPCMBuffer(pcmFormat: outputFormat, frameCapacity: capacity)!
        var supplied = false
        var failure: NSError?
        let result = converter.convert(to: output, error: &failure) { _, status in
            if supplied { status.pointee = .noDataNow; return nil }
            supplied = true
            status.pointee = .haveData
            return pcm
        }
        guard result != .error, failure == nil else { throw failure ?? CaptureError("PCM conversion failed") as NSError }
        try publish(output)
        previousEnd = end
    }

    func publish(_ output: AVAudioPCMBuffer) throws {
        guard let converter else { return }
        if output.frameLength > 0 {
            pendingPCM.append(Data(bytes: output.int16ChannelData![0], count: Int(output.frameLength) * 2))
        }
        let target = Int64(floor(Double(inputFrames) * 24_000 / converter.inputFormat.sampleRate))
        while let timing = timings.first {
            let boundary = Int64(ceil(Double(timing.firstFrame + timing.frames) * 24_000 / converter.inputFormat.sampleRate))
            let count = min(Int64(pendingPCM.count / 2), target - outputFrames, boundary - outputFrames)
            if count <= 0 { break }
            let bytes = Int(count) * 2
            // Keep each captured buffer's clock observation. Nominal sample
            // rates can drift relative to host time over a long meeting.
            let start = timing.start + Double(outputFrames) / 24_000 - Double(timing.firstFrame) / converter.inputFormat.sampleRate
            let end = min(timing.start + Double(timing.frames) / converter.inputFormat.sampleRate,
                          timing.start + Double(outputFrames + count) / 24_000 - Double(timing.firstFrame) / converter.inputFormat.sampleRate)
            try audio.write(contentsOf: pendingPCM.prefix(bytes))
            try timeline.write(contentsOf: jsonLine([
                "offset": offset, "bytes": bytes, "start_seconds": start, "end_seconds": end,
                "discontinuity": pendingGap,
            ]))
            offset += Int64(bytes)
            outputFrames += count
            pendingGap = false
            pendingPCM.removeFirst(bytes)
            if outputFrames == boundary { timings.removeFirst() }
        }
    }

    func drain() throws {
        guard let converter else { return }
        while true {
            let output = AVAudioPCMBuffer(pcmFormat: outputFormat, frameCapacity: 4096)!
            var failure: NSError?
            let result = converter.convert(to: output, error: &failure) { _, status in
                status.pointee = .endOfStream
                return nil
            }
            guard result != .error, failure == nil else { throw failure ?? CaptureError("PCM drain failed") as NSError }
            try publish(output)
            if result == .endOfStream { break }
            guard output.frameLength > 0 else { throw CaptureError("PCM drain made no progress") }
        }
        let expected = Int64(floor(Double(inputFrames) * 24_000 / converter.inputFormat.sampleRate))
        guard outputFrames == expected else { throw CaptureError("PCM converter did not flush its final samples") }
    }

    func finish() throws {
        try drain()
        try audio.synchronize()
        try timeline.synchronize()
        try audio.close()
        try timeline.close()
    }

    func abort() throws {
        // A journal failure may follow a successful PCM write. Never drain and
        // append those pending bytes again. Retry reconstructs unpublished data.
        try audio.close()
        try timeline.close()
    }
}

final class CaptureOutput: NSObject, SCStreamOutput, SCStreamDelegate, @unchecked Sendable {
    let queue = DispatchQueue(label: "local.trailmix.capture.audio")
    let origin: CMTime
    let system: TrackWriter
    let microphone: TrackWriter
    let stop: AsyncStream<Void>.Continuation
    private var failure: String?
    private var closed = false

    init(directory: URL, stop: AsyncStream<Void>.Continuation, streaming: Bool) throws {
        self.origin = CMClockGetTime(CMClockGetHostTimeClock())
        self.stop = stop
        system = try TrackWriter(name: "system", directory: directory, streaming: streaming)
        microphone = try TrackWriter(name: "microphone", directory: directory, streaming: streaming)
    }

    var elapsed: Double {
        CMTimeGetSeconds(CMTimeSubtract(CMClockGetTime(CMClockGetHostTimeClock()), origin))
    }

    func report(_ type: String) {
        guard !closed || type == "stopped" else { return }
        emit(["type": type, "elapsed_seconds": elapsed,
              "tracks": ["system": system.stats, "microphone": microphone.stats]])
    }

    func fail(_ message: String) {
        queue.async { self.failOnQueue(message) }
    }

    private func failOnQueue(_ message: String) {
        guard failure == nil, !closed else { return }
        failure = message
        emit(["type": "error", "error": message])
        stop.yield(())
    }

    func stream(_ stream: SCStream, didStopWithError error: any Error) {
        fail("Capture stopped: \(error.localizedDescription)")
    }

    func stream(_ stream: SCStream, didOutputSampleBuffer sampleBuffer: CMSampleBuffer, of type: SCStreamOutputType) {
        guard !closed, failure == nil, sampleBuffer.isValid,
              CMSampleBufferDataIsReady(sampleBuffer) else { return }
        let track: TrackWriter
        switch type {
        case .audio: track = system
        case .microphone: track = microphone
        default: return
        }
        do {
            guard let clock = stream.synchronizationClock else {
                throw CaptureError("Capture synchronization clock is unavailable")
            }
            let hostPTS = CMSyncConvertTime(
                sampleBuffer.presentationTimeStamp, from: clock, to: CMClockGetHostTimeClock()
            )
            let seconds = CMTimeGetSeconds(CMTimeSubtract(hostPTS, origin))
            try track.write(sampleBuffer, startSeconds: seconds)
        } catch {
            failOnQueue(error.localizedDescription)
        }
    }

    func finish() throws {
        try queue.sync {
            closed = true
            try system.finish()
            try microphone.finish()
            report("stopped")
            if let failure { throw CaptureError(failure) }
        }
    }
}

@main
struct CaptureMain {
    static func main() async {
        do {
            let args = Array(CommandLine.arguments.dropFirst())
            switch args.first {
            case "devices":
                let selected = AVCaptureDevice.default(for: .audio)?.uniqueID
                let devices: [[String: Any]] = microphones().map {
                    ["id": $0.uniqueID, "name": $0.localizedName, "default": $0.uniqueID == selected]
                }
                emit(["microphones": devices])
            case "record":
                var output: String?
                var mic: String?
                var streaming = false
                var i = 1
                while i < args.count {
                    guard i + 1 < args.count else { throw CaptureError("Missing option value") }
                    switch args[i] {
                    case "--output": output = args[i + 1]
                    case "--mic": mic = args[i + 1].isEmpty ? nil : args[i + 1]
                    case "--stream": streaming = args[i + 1] == "true"
                    default: throw CaptureError("Unknown option \(args[i])")
                    }
                    i += 2
                }
                guard let output else { throw CaptureError("--output is required") }
                try await record(directory: URL(fileURLWithPath: output), microphoneID: mic, streaming: streaming)
            case "permissions":
                // Report without prompting; the first recording asks macOS.
                let microphone: String
                switch AVCaptureDevice.authorizationStatus(for: .audio) {
                case .authorized: microphone = "authorized"
                case .denied: microphone = "denied"
                case .restricted: microphone = "restricted"
                case .notDetermined: microphone = "not_determined"
                @unknown default: microphone = "unknown"
                }
                emit(["microphone": microphone, "screen_capture": CGPreflightScreenCaptureAccess()])
            case "self-test":
                try selfTest()
                emit(["self_test": "passed"])
            case "prepare-stream":
                guard args.count == 3, args[1] == "--output" else { throw CaptureError("Use prepare-stream --output DIR") }
                try prepareStream(directory: URL(fileURLWithPath: args[2]))
            default:
                throw CaptureError("Use devices, permissions, record --output DIR [--mic ID], or self-test")
            }
        } catch {
            emit(["type": "error", "error": error.localizedDescription])
            exit(1)
        }
    }

    // Rebuild the transcription queue from the authoritative CAF and capture
    // journal. The Go CLI holds capture/recorder locks during this operation.
    static func prepareStream(directory: URL) throws {
        let temporary = directory.appendingPathComponent(".stream-repair-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: temporary, withIntermediateDirectories: false)
        defer { try? FileManager.default.removeItem(at: temporary) }
        for name in ["microphone", "system"] {
            let journal = try String(contentsOf: directory.appendingPathComponent("\(name).timeline.jsonl"), encoding: .utf8)
            guard journal.isEmpty || journal.hasSuffix("\n") else { throw CaptureError("\(name): capture journal is truncated; original audio retained") }
            let stream = try StreamWriter(name: name, directory: temporary)
            let rows = journal.split(separator: "\n")
            if !rows.isEmpty {
                let url = directory.appendingPathComponent("\(name).caf")
                let probe = try AVAudioFile(forReading: url)
                let audio = try AVAudioFile(forReading: url, commonFormat: probe.fileFormat.commonFormat, interleaved: probe.fileFormat.isInterleaved)
                for text in rows {
                    guard let row = try JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any],
                          let frame = row["file_frame"] as? Int64, let count = row["frames"] as? Int,
                          let start = row["start_seconds"] as? Double, count > 0, count <= 192_000 else { throw CaptureError("Invalid capture timing row") }
                    audio.framePosition = frame
                    let pcm = AVAudioPCMBuffer(pcmFormat: audio.processingFormat, frameCapacity: AVAudioFrameCount(count))!
                    try audio.read(into: pcm, frameCount: AVAudioFrameCount(count))
                    guard pcm.frameLength == count else { throw CaptureError("CAF is shorter than its timing journal") }
                    try stream.write(pcm, start: start)
                }
            }
            try stream.finish()
            // A retry must not change audio beneath existing passage offsets.
            let oldURL = directory.appendingPathComponent("\(name).stream.pcm")
            if FileManager.default.fileExists(atPath: oldURL.path) {
                let old = try FileHandle(forReadingFrom: oldURL)
                let rebuilt = try FileHandle(forReadingFrom: temporary.appendingPathComponent("\(name).stream.pcm"))
                defer { try? old.close(); try? rebuilt.close() }
                while let chunk = try old.read(upToCount: 65_536), !chunk.isEmpty {
                    guard try rebuilt.read(upToCount: chunk.count) == chunk else { throw CaptureError("\(name): reconstructed PCM differs; existing evidence retained") }
                }
            }
        }
        for name in ["microphone", "system"] {
            for suffix in ["stream.pcm", "stream.jsonl"] {
                let source = temporary.appendingPathComponent("\(name).\(suffix)").path
                let target = directory.appendingPathComponent("\(name).\(suffix)").path
                guard rename(source, target) == 0 else { throw CaptureError("Cannot publish repaired PCM queue") }
            }
            let marker = directory.appendingPathComponent("\(name).stream.error")
            if FileManager.default.fileExists(atPath: marker.path) { try FileManager.default.removeItem(at: marker) }
        }
        emit(["prepared_stream": true])
    }

    static func record(directory: URL, microphoneID: String?, streaming: Bool) async throws {
        // Retry must wait for native finalization even if its Go parent crashed.
        let guardFile = try exclusiveFile(directory.appendingPathComponent("capture.lock"))
        guard flock(guardFile.fileDescriptor, LOCK_EX | LOCK_NB) == 0 else { throw CaptureError("Capture directory is locked") }
        defer { try? guardFile.close() }
        let stops = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
        // EOF is also a stop request, including when the Go parent crashes.
        Thread.detachNewThread {
            _ = readLine()
            stops.continuation.yield(())
        }
        let signals = [SIGINT, SIGTERM].map { number in
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: .global())
            source.setEventHandler { stops.continuation.yield(()) }
            source.resume()
            return source
        }
        defer { signals.forEach { $0.cancel() }; stops.continuation.finish() }

        guard await AVCaptureDevice.requestAccess(for: .audio) else {
            throw CaptureError("Microphone access denied. Enable TrailMix Capture or the launching app in System Settings > Privacy & Security > Microphone, then retry.")
        }
        let selectedID = microphoneID ?? AVCaptureDevice.default(for: .audio)?.uniqueID
        guard let selected = microphones().first(where: { $0.uniqueID == selectedID }) else {
            throw CaptureError("Selected microphone is unavailable. Run trailmix devices and choose a connected input.")
        }
        let content: SCShareableContent
        do {
            content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
        } catch {
            throw CaptureError("Cannot access system audio: \(error.localizedDescription). Check System Settings > Privacy & Security > Screen & System Audio Recording, then retry.")
        }
        guard let display = content.displays.first else { throw CaptureError("No display available for the ScreenCaptureKit filter") }
        let configuration = SCStreamConfiguration()
        configuration.capturesAudio = true
        configuration.captureMicrophone = true
        configuration.microphoneCaptureDeviceID = selected.uniqueID
        configuration.sampleRate = 48_000
        configuration.channelCount = 2
        configuration.excludesCurrentProcessAudio = true
        // ScreenCaptureKit requires a content filter, but no screen output is
        // registered or saved. Keep unused video configuration inexpensive.
        configuration.width = 2
        configuration.height = 2
        configuration.minimumFrameInterval = CMTime(value: 1, timescale: 1)
        configuration.showsCursor = false
        let filter = SCContentFilter(display: display, excludingWindows: [])
        let output = try CaptureOutput(directory: directory, stop: stops.continuation, streaming: streaming)
        let stream = SCStream(filter: filter, configuration: configuration, delegate: output)
        try stream.addStreamOutput(output, type: .audio, sampleHandlerQueue: output.queue)
        try stream.addStreamOutput(output, type: .microphone, sampleHandlerQueue: output.queue)
        let observer = NotificationCenter.default.addObserver(
            forName: AVCaptureDevice.wasDisconnectedNotification, object: selected, queue: nil
        ) { _ in output.fail("Selected microphone disconnected. Reconnect or select another microphone and start a new recording.") }
        defer { NotificationCenter.default.removeObserver(observer) }
        let metadata = try exclusiveFile(directory.appendingPathComponent("capture.json"))
        try metadata.write(contentsOf: jsonLine([
            "schema_version": 1,
            "origin_host_seconds": CMTimeGetSeconds(output.origin),
            "microphone_id": selected.uniqueID,
            "microphone_name": selected.localizedName,
            "clock": "SCStream synchronizationClock converted to CMClock host time",
            "system_scope": "all system audio except capture helper",
        ]))
        try metadata.close()
        try await stream.startCapture()
        output.queue.sync { output.report("started") }
        let timer = DispatchSource.makeTimerSource(queue: output.queue)
        timer.schedule(deadline: .now() + 1, repeating: 1)
        timer.setEventHandler { output.report("progress") }
        timer.resume()
        for await _ in stops.stream { break }
        timer.cancel()
        do {
            try await stream.stopCapture()
        } catch {
            output.fail("Could not stop capture cleanly: \(error.localizedDescription)")
        }
        try output.finish()
    }

    // Real PCM buffers exercise file writing and the file-frame/meeting-time
    // mapping without opening audio devices or requesting permissions.
    static func selfTest() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
        defer { try? FileManager.default.removeItem(at: directory) }
        let format = AVAudioFormat(standardFormatWithSampleRate: 48_000, channels: 2)!
        let pcm = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: 480)!
        pcm.frameLength = 480
        for channel in 0..<2 {
            for frame in 0..<480 { pcm.floatChannelData![channel][frame] = 0.25 }
        }
        let sample = try testSample(pcm)
        let track = try TrackWriter(name: "test", directory: directory, streaming: true)
        try track.write(sample, startSeconds: 1.25)
        try track.write(sample, startSeconds: 1.26)
        try track.write(sample, startSeconds: 2.0)
        try track.finish()
        let audio = try AVAudioFile(forReading: directory.appendingPathComponent("test.caf"))
        guard audio.length == 1440, track.discontinuities == 1,
              track.firstSeconds == 1.25, abs(track.endSeconds - 2.01) < 0.000_001 else {
            throw CaptureError("PCM length or timeline mapping test failed")
        }
        let replay = AVAudioPCMBuffer(pcmFormat: audio.processingFormat, frameCapacity: 1440)!
        try audio.read(into: replay)
        guard replay.floatChannelData![1][1000] == 0.25 else { throw CaptureError("PCM content changed") }
        let rows = try String(contentsOf: directory.appendingPathComponent("test.timeline.jsonl"), encoding: .utf8)
            .split(separator: "\n")
        let last = try JSONSerialization.jsonObject(with: Data(rows[2].utf8)) as! [String: Any]
        guard last["file_frame"] as? Int == 960,
              abs((last["discontinuity_seconds"] as? Double ?? 0) - 0.73) < 0.000_001 else {
            throw CaptureError("Timeline did not preserve missing audio range")
        }
        do {
            _ = try TrackWriter(name: "test", directory: directory)
            throw CaptureError("Existing recording was reopened")
        } catch let error as CaptureError {
            guard error.message.hasPrefix("Cannot create") else { throw error }
        }

        // The WH-1000XM5 delivers Int16 PCM, unlike the built-in Float32 mic.
        let headsetFormat = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: 16_000, channels: 1, interleaved: true)!
        let headsetPCM = AVAudioPCMBuffer(pcmFormat: headsetFormat, frameCapacity: 160)!
        headsetPCM.frameLength = 160
        for frame in 0..<160 { headsetPCM.int16ChannelData![0][frame] = -8192 }
        let headset = try TrackWriter(name: "headset", directory: directory, streaming: true)
        try headset.write(testSample(headsetPCM), startSeconds: 0.1)
        try headset.finish()
        let headsetAudio = try AVAudioFile(forReading: directory.appendingPathComponent("headset.caf"))
        let headsetReplay = AVAudioPCMBuffer(pcmFormat: headsetAudio.processingFormat, frameCapacity: 160)!
        try headsetAudio.read(into: headsetReplay)
        guard headsetReplay.floatChannelData![0][100] == -0.25 else {
            throw CaptureError("Int16 microphone audio was corrupted or silenced by the writer")
        }
        emit(["self_test_int16_file": "passed"])
        guard headset.peak == 0.25 else {
            throw CaptureError("Int16 microphone peak incorrectly reported as \(headset.peak), expected 0.25")
        }
        for (name, expectedBytes, expectedSample) in [("test", 1440, Int16(8192)), ("headset", 480, Int16(-8192))] {
            let data = try Data(contentsOf: directory.appendingPathComponent("\(name).stream.pcm"))
            guard data.count == expectedBytes else { throw CaptureError("\(name): resampled length \(data.count), expected \(expectedBytes)") }
            let sample = data.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 200, as: Int16.self) }
            guard abs(Int(sample) - Int(expectedSample)) < 64 else { throw CaptureError("\(name): converted PCM level changed") }
        }
        let streamRows = try String(contentsOf: directory.appendingPathComponent("test.stream.jsonl"), encoding: .utf8).split(separator: "\n")
        let streamGapRows = try streamRows.map { try JSONSerialization.jsonObject(with: Data($0.utf8)) as! [String: Any] }
        guard streamGapRows.contains(where: { $0["offset"] as? Int == 960 && $0["start_seconds"] as? Double == 2 && $0["discontinuity"] as? Bool == true }) else { throw CaptureError("Resampling lost capture gap mapping") }
        // A non-integer rate ratio must not accumulate sample-count drift.
        let oddFormat = AVAudioFormat(standardFormatWithSampleRate: 44_100, channels: 1)!
        let oddPCM = AVAudioPCMBuffer(pcmFormat: oddFormat, frameCapacity: 441)!
        oddPCM.frameLength = 441
        for frame in 0..<441 { oddPCM.floatChannelData![0][frame] = 0.1 }
        let odd = try StreamWriter(name: "44k", directory: directory)
        for index in 0..<100 { try odd.write(oddPCM, start: Double(index) / 100) }
        try odd.finish()
        guard odd.offset == 48_000 else { throw CaptureError("44.1kHz resampling drifted: \(odd.offset) bytes") }
        let irregular = AVAudioPCMBuffer(pcmFormat: oddFormat, frameCapacity: 512)!
        irregular.frameLength = 512
        for frame in 0..<512 { irregular.floatChannelData![0][frame] = frame % 53 == 0 ? 0.5 : 0 }
        let split = try StreamWriter(name: "split", directory: directory)
        for index in 0..<100 { try split.write(irregular, start: Double(index * 512) / 44_100) }
        try split.finish()
        let combined = AVAudioPCMBuffer(pcmFormat: oddFormat, frameCapacity: 51_200)!
        combined.frameLength = 51_200
        for frame in 0..<51_200 { combined.floatChannelData![0][frame] = irregular.floatChannelData![0][frame % 512] }
        let whole = try StreamWriter(name: "whole", directory: directory)
        try whole.write(combined, start: 0)
        try whole.finish()
        let splitData = try Data(contentsOf: directory.appendingPathComponent("split.stream.pcm"))
        let wholeData = try Data(contentsOf: directory.appendingPathComponent("whole.stream.pcm"))
        guard splitData == wholeData, split.offset == Int64(floor(51_200.0 * 24_000 / 44_100)) * 2 else { throw CaptureError("Resampling depended on callback chunk boundaries") }
        let drift = try StreamWriter(name: "drift", directory: directory)
        for index in 0..<100 { try drift.write(oddPCM, start: Double(index) * 0.0101) }
        try drift.finish()
        let driftRows = try String(contentsOf: directory.appendingPathComponent("drift.stream.jsonl"), encoding: .utf8).split(separator: "\n")
        let driftLast = try JSONSerialization.jsonObject(with: Data(driftRows.last!.utf8)) as! [String: Any]
        guard abs((driftLast["end_seconds"] as? Double ?? 0) - 1.0099) < 0.000_001 else { throw CaptureError("Resampling discarded capture-clock drift") }

        // Verify filter-tail flushing and reconstruction from authoritative CAF.
        let repairDirectory = directory.appendingPathComponent("repair")
        try FileManager.default.createDirectory(at: repairDirectory, withIntermediateDirectories: false)
        for frame in 0..<160 { headsetPCM.int16ChannelData![0][frame] = 0 }
        headsetPCM.int16ChannelData![0][159] = 16_000
        let local = try TrackWriter(name: "microphone", directory: repairDirectory, streaming: true)
        let remote = try TrackWriter(name: "system", directory: repairDirectory, streaming: true)
        try local.write(testSample(headsetPCM), startSeconds: 1)
        try remote.write(sample, startSeconds: 2)
        try local.finish(); try remote.finish()
        let localBytes = try Data(contentsOf: repairDirectory.appendingPathComponent("microphone.stream.pcm"))
        guard localBytes.suffix(48).contains(where: { $0 != 0 }) else { throw CaptureError("Resampler lost final speech samples") }
        try prepareStream(directory: repairDirectory)
        let repairedBytes = try Data(contentsOf: repairDirectory.appendingPathComponent("microphone.stream.pcm"))
        guard repairedBytes == localBytes else { throw CaptureError("CAF reconstruction changed provider audio") }
        let failureDirectory = directory.appendingPathComponent("queue-failure")
        try FileManager.default.createDirectory(at: failureDirectory, withIntermediateDirectories: false)
        let failed = try TrackWriter(name: "microphone", directory: failureDirectory, streaming: true)
        let empty = try TrackWriter(name: "system", directory: failureDirectory)
        try failed.stream!.timeline.close()
        try failed.write(testSample(headsetPCM), startSeconds: 1)
        try failed.finish(); try empty.finish()
        guard failed.streamError != nil else { throw CaptureError("Injected queue failure was not reported") }
        try prepareStream(directory: failureDirectory)
        let recovered = try Data(contentsOf: failureDirectory.appendingPathComponent("microphone.stream.pcm"))
        guard recovered == localBytes else { throw CaptureError("Queue failure prevented recovery from original CAF") }
    }

    static func testSample(_ pcm: AVAudioPCMBuffer) throws -> CMSampleBuffer {
        var description: CMAudioFormatDescription?
        var asbd = pcm.format.streamDescription.pointee
        guard CMAudioFormatDescriptionCreate(
            allocator: kCFAllocatorDefault, asbd: &asbd, layoutSize: 0, layout: nil,
            magicCookieSize: 0, magicCookie: nil, extensions: nil, formatDescriptionOut: &description
        ) == noErr else { throw CaptureError("Test format creation failed") }
        var timing = CMSampleTimingInfo(
            duration: CMTime(value: 1, timescale: Int32(pcm.format.sampleRate)),
            presentationTimeStamp: .zero, decodeTimeStamp: .invalid
        )
        var sample: CMSampleBuffer?
        guard CMSampleBufferCreate(
            allocator: kCFAllocatorDefault, dataBuffer: nil, dataReady: false,
            makeDataReadyCallback: nil, refcon: nil, formatDescription: description,
            sampleCount: Int(pcm.frameLength), sampleTimingEntryCount: 1, sampleTimingArray: &timing,
            sampleSizeEntryCount: 0, sampleSizeArray: nil, sampleBufferOut: &sample
        ) == noErr, let sample else { throw CaptureError("Test sample creation failed") }
        guard CMSampleBufferSetDataBufferFromAudioBufferList(
            sample, blockBufferAllocator: kCFAllocatorDefault,
            blockBufferMemoryAllocator: kCFAllocatorDefault, flags: 0,
            bufferList: pcm.audioBufferList
        ) == noErr else { throw CaptureError("Test sample data creation failed") }
        return sample
    }
}
