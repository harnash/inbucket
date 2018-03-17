package file

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhillyerd/inbucket/pkg/config"
	"github.com/jhillyerd/inbucket/pkg/log"
	"github.com/jhillyerd/inbucket/pkg/policy"
	"github.com/jhillyerd/inbucket/pkg/storage"
	"github.com/jhillyerd/inbucket/pkg/stringutil"
)

// Name of index file in each mailbox
const indexFileName = "index.gob"

var (
	// indexMx is locked while reading/writing an index file
	//
	// NOTE: This is a bottleneck because it's a single lock even if we have a
	// million index files
	indexMx = new(sync.RWMutex)

	// dirMx is locked while creating/removing directories
	dirMx = new(sync.Mutex)

	// countChannel is filled with a sequential numbers (0000..9999), which are
	// used by generateID() to generate unique message IDs.  It's global
	// because we only want one regardless of the number of DataStore objects
	countChannel = make(chan int, 10)
)

func init() {
	// Start generator
	go countGenerator(countChannel)
}

// Populates the channel with numbers
func countGenerator(c chan int) {
	for i := 0; true; i = (i + 1) % 10000 {
		c <- i
	}
}

// Store implements DataStore aand is the root of the mail storage
// hiearchy.  It provides access to Mailbox objects
type Store struct {
	hashLock   storage.HashLock
	path       string
	mailPath   string
	messageCap int
}

// New creates a new DataStore object using the specified path
func New(cfg config.DataStoreConfig) storage.Store {
	path := cfg.Path
	if path == "" {
		log.Errorf("No value configured for datastore path")
		return nil
	}
	mailPath := filepath.Join(path, "mail")
	if _, err := os.Stat(mailPath); err != nil {
		// Mail datastore does not yet exist
		if err = os.MkdirAll(mailPath, 0770); err != nil {
			log.Errorf("Error creating dir %q: %v", mailPath, err)
		}
	}
	return &Store{path: path, mailPath: mailPath, messageCap: cfg.MailboxMsgCap}
}

// AddMessage adds a message to the specified mailbox.
func (fs *Store) AddMessage(m storage.StoreMessage) (id string, err error) {
	r, err := m.RawReader()
	if err != nil {
		return "", err
	}
	mb, err := fs.mbox(m.Mailbox())
	if err != nil {
		return "", err
	}
	// Create a new message.
	fm, err := mb.newMessage()
	if err != nil {
		return "", err
	}
	// Ensure mailbox directory exists.
	if err := mb.createDir(); err != nil {
		return "", err
	}
	// Write the message content
	file, err := os.Create(fm.rawPath())
	if err != nil {
		return "", err
	}
	w := bufio.NewWriter(file)
	size, err := io.Copy(w, r)
	if err != nil {
		// Try to remove the file
		_ = file.Close()
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	_ = r.Close()
	if err := w.Flush(); err != nil {
		// Try to remove the file
		_ = file.Close()
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	if err := file.Close(); err != nil {
		// Try to remove the file
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	// Update the index.
	fm.Fdate = m.Date()
	fm.Ffrom = m.From()
	fm.Fto = m.To()
	fm.Fsize = size
	fm.Fsubject = m.Subject()
	mb.messages = append(mb.messages, fm)
	if err := mb.writeIndex(); err != nil {
		// Try to remove the file
		_ = os.Remove(fm.rawPath())
		return "", err
	}
	return fm.Fid, nil
}

// GetMessage returns the messages in the named mailbox, or an error.
func (fs *Store) GetMessage(mailbox, id string) (storage.StoreMessage, error) {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mb.getMessage(id)
}

// GetMessages returns the messages in the named mailbox, or an error.
func (fs *Store) GetMessages(mailbox string) ([]storage.StoreMessage, error) {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mb.getMessages()
}

// RemoveMessage deletes a message by ID from the specified mailbox.
func (fs *Store) RemoveMessage(mailbox, id string) error {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return err
	}
	return mb.removeMessage(id)
}

// PurgeMessages deletes all messages in the named mailbox, or returns an error.
func (fs *Store) PurgeMessages(mailbox string) error {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return err
	}
	return mb.purge()
}

// VisitMailboxes accepts a function that will be called with the messages in each mailbox while it
// continues to return true.
func (fs *Store) VisitMailboxes(f func([]storage.StoreMessage) (cont bool)) error {
	infos1, err := ioutil.ReadDir(fs.mailPath)
	if err != nil {
		return err
	}
	// Loop over level 1 directories
	for _, inf1 := range infos1 {
		if inf1.IsDir() {
			l1 := inf1.Name()
			infos2, err := ioutil.ReadDir(filepath.Join(fs.mailPath, l1))
			if err != nil {
				return err
			}
			// Loop over level 2 directories
			for _, inf2 := range infos2 {
				if inf2.IsDir() {
					l2 := inf2.Name()
					infos3, err := ioutil.ReadDir(filepath.Join(fs.mailPath, l1, l2))
					if err != nil {
						return err
					}
					// Loop over mailboxes
					for _, inf3 := range infos3 {
						if inf3.IsDir() {
							mbdir := inf3.Name()
							mbpath := filepath.Join(fs.mailPath, l1, l2, mbdir)
							idx := filepath.Join(mbpath, indexFileName)
							mb := &mbox{store: fs, dirName: mbdir, path: mbpath,
								indexPath: idx}
							msgs, err := mb.getMessages()
							if err != nil {
								return err
							}
							if !f(msgs) {
								return nil
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// LockFor returns the RWMutex for this mailbox, or an error.
func (fs *Store) LockFor(emailAddress string) (*sync.RWMutex, error) {
	name, err := policy.ParseMailboxName(emailAddress)
	if err != nil {
		return nil, err
	}
	hash := stringutil.HashMailboxName(name)
	return fs.hashLock.Get(hash), nil
}

// NewMessage is temproary until #69 MessageData refactor
func (fs *Store) NewMessage(mailbox string) (storage.StoreMessage, error) {
	mb, err := fs.mbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mb.newMessage()
}

// mbox returns the named mailbox.
func (fs *Store) mbox(mailbox string) (*mbox, error) {
	name, err := policy.ParseMailboxName(mailbox)
	if err != nil {
		return nil, err
	}
	dir := stringutil.HashMailboxName(name)
	s1 := dir[0:3]
	s2 := dir[0:6]
	path := filepath.Join(fs.mailPath, s1, s2, dir)
	indexPath := filepath.Join(path, indexFileName)

	return &mbox{store: fs, name: name, dirName: dir, path: path,
		indexPath: indexPath}, nil
}

// mbox manages the mail for a specific user and correlates to a particular directory on disk.
type mbox struct {
	store       *Store
	name        string
	dirName     string
	path        string
	indexLoaded bool
	indexPath   string
	messages    []*Message
}

// getMessages scans the mailbox directory for .gob files and decodes them into
// a slice of Message objects.
func (mb *mbox) getMessages() ([]storage.StoreMessage, error) {
	if !mb.indexLoaded {
		if err := mb.readIndex(); err != nil {
			return nil, err
		}
	}
	messages := make([]storage.StoreMessage, len(mb.messages))
	for i, m := range mb.messages {
		messages[i] = m
	}
	return messages, nil
}

// getMessage decodes a single message by ID and returns a Message object.
func (mb *mbox) getMessage(id string) (storage.StoreMessage, error) {
	if !mb.indexLoaded {
		if err := mb.readIndex(); err != nil {
			return nil, err
		}
	}
	if id == "latest" && len(mb.messages) != 0 {
		return mb.messages[len(mb.messages)-1], nil
	}
	for _, m := range mb.messages {
		if m.Fid == id {
			return m, nil
		}
	}
	return nil, storage.ErrNotExist
}

// removeMessage deletes the message off disk and removes it from the index.
func (mb *mbox) removeMessage(id string) error {
	if !mb.indexLoaded {
		if err := mb.readIndex(); err != nil {
			return err
		}
	}
	var msg *Message
	for i, m := range mb.messages {
		if id == m.ID() {
			msg = m
			// Slice around message we are deleting
			mb.messages = append(mb.messages[:i], mb.messages[i+1:]...)
			break
		}
	}
	if msg == nil {
		return storage.ErrNotExist
	}
	if err := mb.writeIndex(); err != nil {
		return err
	}
	if len(mb.messages) == 0 {
		// This was the last message, thus writeIndex() has removed the entire
		// directory; we don't need to delete the raw file.
		return nil
	}
	// There are still messages in the index
	log.Tracef("Deleting %v", msg.rawPath())
	return os.Remove(msg.rawPath())
}

// purge deletes all messages in this mailbox.
func (mb *mbox) purge() error {
	mb.messages = mb.messages[:0]
	return mb.writeIndex()
}

// readIndex loads the mailbox index data from disk
func (mb *mbox) readIndex() error {
	// Clear message slice, open index
	mb.messages = mb.messages[:0]
	// Lock for reading
	indexMx.RLock()
	defer indexMx.RUnlock()
	// Check if index exists
	if _, err := os.Stat(mb.indexPath); err != nil {
		// Does not exist, but that's not an error in our world
		log.Tracef("Index %v does not exist (yet)", mb.indexPath)
		mb.indexLoaded = true
		return nil
	}
	file, err := os.Open(mb.indexPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Errorf("Failed to close %q: %v", mb.indexPath, err)
		}
	}()
	// Decode gob data
	dec := gob.NewDecoder(bufio.NewReader(file))
	name := ""
	if err = dec.Decode(&name); err != nil {
		return fmt.Errorf("Corrupt mailbox %q: %v", mb.indexPath, err)
	}
	mb.name = name
	for {
		// Load messages until EOF
		msg := &Message{}
		if err = dec.Decode(msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("Corrupt mailbox %q: %v", mb.indexPath, err)
		}
		msg.mailbox = mb
		mb.messages = append(mb.messages, msg)
	}
	mb.indexLoaded = true
	return nil
}

// writeIndex overwrites the index on disk with the current mailbox data
func (mb *mbox) writeIndex() error {
	// Lock for writing
	indexMx.Lock()
	defer indexMx.Unlock()
	if len(mb.messages) > 0 {
		// Ensure mailbox directory exists
		if err := mb.createDir(); err != nil {
			return err
		}
		// Open index for writing
		file, err := os.Create(mb.indexPath)
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(file)
		// Write each message and then flush
		enc := gob.NewEncoder(writer)
		if err = enc.Encode(mb.name); err != nil {
			_ = file.Close()
			return err
		}
		for _, m := range mb.messages {
			if err = enc.Encode(m); err != nil {
				_ = file.Close()
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			log.Errorf("Failed to close %q: %v", mb.indexPath, err)
			return err
		}
	} else {
		// No messages, delete index+maildir
		log.Tracef("Removing mailbox %v", mb.path)
		return mb.removeDir()
	}
	return nil
}

// createDir checks for the presence of the path for this mailbox, creates it if needed
func (mb *mbox) createDir() error {
	dirMx.Lock()
	defer dirMx.Unlock()
	if _, err := os.Stat(mb.path); err != nil {
		if err := os.MkdirAll(mb.path, 0770); err != nil {
			log.Errorf("Failed to create directory %v, %v", mb.path, err)
			return err
		}
	}
	return nil
}

// removeDir removes the mailbox, plus empty higher level directories
func (mb *mbox) removeDir() error {
	dirMx.Lock()
	defer dirMx.Unlock()
	// remove mailbox dir, including index file
	if err := os.RemoveAll(mb.path); err != nil {
		return err
	}
	// remove parents if empty
	dir := filepath.Dir(mb.path)
	if removeDirIfEmpty(dir) {
		removeDirIfEmpty(filepath.Dir(dir))
	}
	return nil
}

// removeDirIfEmpty will remove the specified directory if it contains no files or directories.
// Caller should hold dirMx.  Returns true if dir was removed.
func removeDirIfEmpty(path string) (removed bool) {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	files, err := f.Readdirnames(0)
	_ = f.Close()
	if err != nil {
		return false
	}
	if len(files) > 0 {
		// Dir not empty
		return false
	}
	log.Tracef("Removing dir %v", path)
	err = os.Remove(path)
	if err != nil {
		log.Errorf("Failed to remove %q: %v", path, err)
		return false
	}
	return true
}

// generatePrefix converts a Time object into the ISO style format we use
// as a prefix for message files.  Note:  It is used directly by unit
// tests.
func generatePrefix(date time.Time) string {
	return date.Format("20060102T150405")
}

// generateId adds a 4-digit unique number onto the end of the string
// returned by generatePrefix()
func generateID(date time.Time) string {
	return generatePrefix(date) + "-" + fmt.Sprintf("%04d", <-countChannel)
}
