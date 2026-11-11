// Package checkpoints is the Controller side of capturing a checkpoint of a
// Run (§11.7, §23): the request that puts the Run in checkpointing and orders
// its Worker to capture, the report that the capture is done or failed, and
// the arbitration against an exit or a cancel that comes meanwhile.
package checkpoints
