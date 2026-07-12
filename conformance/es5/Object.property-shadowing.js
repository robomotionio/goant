function check(a,b){if(a!==b)throw a+" !== "+b;} var p={x:1};var o=Object.create(p);o.x=2;check(o.x,2);check(p.x,1);delete o.x;check(o.x,1);console.log('PASS');
