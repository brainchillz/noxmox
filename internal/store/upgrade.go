package store

import "fmt"

// WriteSuper writes s to both superblock locations and adopts it. The
// caller has made sure every region s refers to is initialized.
func (p *Pool) WriteSuper(s *Super) error {
	if s.DevSize != p.Dev.Size {
		return fmt.Errorf("superblock is for a %d byte device, this one is %d", s.DevSize, p.Dev.Size)
	}
	b := s.encode()
	if err := p.Dev.WriteAt(0, b); err != nil {
		return err
	}
	if err := p.Dev.WriteAt(p.Dev.Size-SBSize, b); err != nil {
		return err
	}
	p.Super = s
	return nil
}

// ReloadSuper re-reads the superblock, so a long-running daemon picks up
// an upgrade done by another host. Returns true when it changed.
func (p *Pool) ReloadSuper() (bool, error) {
	q, err := Open(p.Dev)
	if err != nil {
		return false, err
	}
	changed := q.Super.Version != p.Super.Version || q.Super.Features != p.Super.Features || q.Super.MailboxOff != p.Super.MailboxOff
	if changed {
		p.Super = q.Super
	}
	return changed, nil
}

// PrepareUpgrade zeroes the regions a v2 superblock will add and returns
// that superblock without writing it. The caller initializes the mailbox
// lease through sanlock and then calls WriteSuper. Nothing that exists is
// touched, so a crash in between leaves a valid v1 pool.
func (p *Pool) PrepareUpgrade(tool string) (*Super, error) {
	n, err := p.Super.UpgradeLayout()
	if err != nil {
		return nil, err
	}
	n.Tool = tool
	if err := p.Dev.Zero(n.MailboxLeaseOff, MiB); err != nil {
		return nil, err
	}
	if err := p.Dev.Zero(n.MailboxOff, n.MailboxSize()); err != nil {
		return nil, err
	}
	return n, nil
}
