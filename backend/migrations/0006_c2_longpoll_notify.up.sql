-- open-picpak backend - schema v6 (Design 16): C2 long-poll wakeup via LISTEN/NOTIFY.
--
-- The C2 handler can long-poll (?wait=N): it holds the request until a command for the polling device
-- lands instead of returning 204 immediately. A trigger fires pg_notify on every command_queue insert;
-- a single process-wide LISTEN connection fans the signal out in-process to every waiting handler, so
-- the whole fleet shares one DB connection for command-arrival signalling.
-- Policy-as-data unaffected: command scripts stay data. golang-migrate convention; forward-only;
-- idempotent (CREATE OR REPLACE / DROP IF EXISTS).

CREATE OR REPLACE FUNCTION command_queue_notify() RETURNS trigger AS $$
BEGIN
    -- payload = the target serial (or '*' = fleet). A waiter re-queries the queue on any wake, so an
    -- over-broad signal is only a cheap redundant query -- never a missed command.
    PERFORM pg_notify('c2_cmd', NEW.serial);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS command_queue_notify_ins ON command_queue;
CREATE TRIGGER command_queue_notify_ins
    AFTER INSERT ON command_queue
    FOR EACH ROW EXECUTE FUNCTION command_queue_notify();
