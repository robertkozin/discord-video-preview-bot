let w, h = @Dimensions()
let size = w - 96
let d = (w - size) / 2

let colors = @GetPalette(art)
@DrawRectangle(0, 0, w, h)
@SetLinearGradient((0, 0), (0, h), "pad", [
    (0.4 , colors[0]),
    (1.0, colors[0] - @rgba(150, 150, 150, 0.0)),
])
@Fill()

@SetFilter("good")
@DrawRoundedRectangle(d, d, size, size, 5.0)
@Clip()
@DrawImageSized(art, d, d, size, size)
@ResetClip()

@SetFont("ggsans-bold")

let offset = size + d*2
@DrawTextBox(track_name, w / 2, offset, 0.5, 0.5, w-d, d)

@SetFont("discord")
@DrawTextBox(artist_name, w / 2, offset + d, 0.5, 0.5, w-d, 35)

@SetColor(#0000002d)
@SetStrokeWidth(3)
@StrokePreserve()

@SetColor(#ffffff)
@Fill()

let s = 10
let spacing = 1
let x = w - @len(colors) * (s + spacing) - d
let y = size + d

for c in colors {
    @DrawRectangle(x, y, s, s)
    @SetColor(c)
    @Fill()

    x = x + s + spacing
}