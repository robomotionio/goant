function check(a,b){if(a!==b)throw a+" !== "+b;} var o={'key':'v','a b':'c'};check(o.key,'v');check(o['a b'],'c');console.log('PASS');
