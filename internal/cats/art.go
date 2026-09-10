package cats

// Art is the cat the client draws at startup, from https://asciiart.website/art/7401,
// where it is signed "hjw" for Hayley Jane Wakenshaw. The signature is not drawn here.
//
// The lines are otherwise as they were drawn, ragged right edge and all. A caller that
// needs every row the same width pads it; nothing here changes the artwork to suit a
// renderer.
var Art = []string{
	`       /\_/\  /\`,
	`      / o o \ \ \`,
	`     /   Y   \/ /`,
	`    /         \/`,
	`    \ | | | | /`,
	"     `|_|-|_|'",
}
