#!/usr/bin/env swift
import AppKit
import Foundation

// Deterministic vector artwork for the native app icon. No external assets.
let destination = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "Resources/Assets.xcassets/AppIcon.appiconset"
try FileManager.default.createDirectory(atPath: destination, withIntermediateDirectories: true)
let specifications = [(16, 1), (16, 2), (32, 1), (32, 2), (128, 1), (128, 2), (256, 1), (256, 2), (512, 1), (512, 2)]
var images = [[String: String]]()
for (size, scale) in specifications {
    let pixels = size * scale
    let representation = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    let context = NSGraphicsContext(bitmapImageRep: representation)!
    NSGraphicsContext.saveGraphicsState(); NSGraphicsContext.current = context
    context.cgContext.scaleBy(x: CGFloat(pixels) / 1024, y: CGFloat(pixels) / 1024)
    NSColor.clear.setFill(); NSRect(x: 0, y: 0, width: 1024, height: 1024).fill()
    NSColor(calibratedRed: 18/255, green: 21/255, blue: 28/255, alpha: 1).setFill()
    NSBezierPath(roundedRect: NSRect(x: 80, y: 80, width: 864, height: 864), xRadius: 175, yRadius: 175).fill()
    NSColor.white.setStroke()
    let shaft = NSBezierPath(); shaft.lineWidth = 67; shaft.lineCapStyle = .square
    shaft.move(to: NSPoint(x: 314, y: 710)); shaft.line(to: NSPoint(x: 700, y: 324)); shaft.stroke()
    let arrow = NSBezierPath(); arrow.lineWidth = 67; arrow.lineJoinStyle = .miter
    arrow.move(to: NSPoint(x: 390, y: 325)); arrow.line(to: NSPoint(x: 700, y: 325)); arrow.line(to: NSPoint(x: 700, y: 635)); arrow.stroke()
    NSGraphicsContext.restoreGraphicsState()
    let filename = "icon_\(size)x\(size)@\(scale)x.png"
    try representation.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: destination).appendingPathComponent(filename))
    images.append(["idiom": "mac", "size": "\(size)x\(size)", "scale": "\(scale)x", "filename": filename])
}
let contents: [String: Any] = ["images": images, "info": ["author": "enoughtools", "version": 1]]
try JSONSerialization.data(withJSONObject: contents, options: [.prettyPrinted, .sortedKeys]).write(to: URL(fileURLWithPath: destination).appendingPathComponent("Contents.json"))
