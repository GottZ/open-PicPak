-- Revert schema v6: drop the C2 long-poll NOTIFY trigger + function.
DROP TRIGGER IF EXISTS command_queue_notify_ins ON command_queue;
DROP FUNCTION IF EXISTS command_queue_notify();
